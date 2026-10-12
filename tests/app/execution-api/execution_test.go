package execution_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	longrunning "google.golang.org/genproto/googleapis/longrunning"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func fixture(t *testing.T, executor bool) (*harness.Clients, context.Context) {
	components := []harness.Component{harness.App}
	if executor {
		components = append(components, harness.Executor)
	}
	f := harness.Start(t, harness.Options{Components: components})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return f.Clients(harness.App), ctx
}

func TestRequestValidation(t *testing.T) {
	c, ctx := fixture(t, false)
	missing := harness.Digest([]byte("missing command"))
	root, err := c.UploadProto(ctx, "validation", &repb.Directory{})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, "validation", &repb.Action{CommandDigest: missing, InputRootDigest: root})
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		req  *repb.ExecuteRequest
		code codes.Code
	}{
		{"missing_action", &repb.ExecuteRequest{ActionDigest: harness.Digest([]byte("missing action"))}, codes.FailedPrecondition},
		{"missing_command", &repb.ExecuteRequest{InstanceName: "validation", ActionDigest: action}, codes.FailedPrecondition},
		{"priority_high", &repb.ExecuteRequest{ActionDigest: action, ExecutionPolicy: &repb.ExecutionPolicy{Priority: 1001}}, codes.InvalidArgument},
		{"priority_low", &repb.ExecuteRequest{ActionDigest: action, ExecutionPolicy: &repb.ExecutionPolicy{Priority: -1001}}, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.SkipCacheLookup = true
			s, err := c.Execution.Execute(ctx, tc.req)
			if err == nil {
				_, err = s.Recv()
			}
			require.Equal(t, tc.code, status.Code(err), "%v", err)
			t.Logf("%s: %v", tc.name, err)
		})
	}
	for _, name := range []string{"", "not-an-operation", "operations/unknown"} {
		s, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
		if err == nil {
			_, err = s.Recv()
		}
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%q: %v", name, err)
	}
}

func uploadAction(t *testing.T, c *harness.Clients, ctx context.Context, script string, noCache bool) *repb.Digest {
	t.Helper()
	root, err := c.UploadProto(ctx, "streams", &repb.Directory{})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, "streams", &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, "streams", &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second), DoNotCache: noCache})
	require.NoError(t, err)
	return action
}

func finish(t *testing.T, s interface {
	Recv() (*longrunning.Operation, error)
}, name string, digest *repb.Digest) *repb.ExecuteResponse {
	t.Helper()
	var result *repb.ExecuteResponse
	for {
		op, err := s.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.Nil(t, result, "update after terminal operation")
		require.Equal(t, name, op.Name)
		require.Nil(t, op.GetError())
		md := &repb.ExecuteOperationMetadata{}
		require.NoError(t, op.GetMetadata().UnmarshalTo(md))
		require.Equal(t, digest.Hash, md.GetActionDigest().GetHash())
		require.Equal(t, digest.SizeBytes, md.GetActionDigest().GetSizeBytes())
		if op.Done {
			require.Equal(t, repb.ExecutionStage_COMPLETED, md.Stage)
			result = &repb.ExecuteResponse{}
			require.NoError(t, op.GetResponse().UnmarshalTo(result))
			require.Zero(t, result.GetStatus().GetCode(), "%v", result.GetStatus())
		}
	}
	require.NotNil(t, result, "EOF before completion")
	return result
}

func TestStreamsAndCachePolicies(t *testing.T) {
	c, ctx := fixture(t, true)
	d := uploadAction(t, c, ctx, "sleep 2; printf execution-api", false)
	executeCtx, cancel := context.WithCancel(ctx)
	s, err := c.Execution.Execute(executeCtx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, SkipCacheLookup: true, ExecutionPolicy: &repb.ExecutionPolicy{Priority: 1000}, ResultsCachePolicy: &repb.ResultsCachePolicy{Priority: 1}})
	require.NoError(t, err)
	first, err := s.Recv()
	require.NoError(t, err)
	require.NotEmpty(t, first.Name)
	require.False(t, first.Done)
	cancel() // Disconnecting Execute must not cancel the action.
	waits := make([]repb.Execution_WaitExecutionClient, 2)
	for i := range waits {
		waits[i], err = c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: first.Name})
		require.NoError(t, err)
	}
	for _, w := range waits {
		r := finish(t, w, first.Name, d)
		require.Zero(t, r.Result.ExitCode)
		require.False(t, r.CachedResult)
	}
	replay, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: first.Name})
	require.NoError(t, err)
	require.Zero(t, finish(t, replay, first.Name, d).Result.ExitCode)
	cached, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, ExecutionPolicy: &repb.ExecutionPolicy{Priority: -1000}, ResultsCachePolicy: &repb.ResultsCachePolicy{Priority: -1}})
	require.NoError(t, err)
	op, err := cached.Recv()
	require.NoError(t, err)
	require.True(t, op.Done)
	require.NotEmpty(t, op.Name)
	response := &repb.ExecuteResponse{}
	require.NoError(t, op.GetResponse().UnmarshalTo(response))
	require.True(t, response.CachedResult)
	require.Zero(t, response.GetStatus().GetCode())
	_, err = cached.Recv()
	require.Equal(t, io.EOF, err)
	// Nonzero process exit is an ActionResult, not an RPC or execution error.
	d = uploadAction(t, c, ctx, "exit 7", true)
	failed, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, SkipCacheLookup: true})
	require.NoError(t, err)
	initial, err := failed.Recv()
	require.NoError(t, err)
	if initial.Done {
		t.Fatal(fmt.Sprintf("expected queued update, got %v", initial))
	}
	require.EqualValues(t, 7, finish(t, failed, initial.Name, d).Result.ExitCode)
}
