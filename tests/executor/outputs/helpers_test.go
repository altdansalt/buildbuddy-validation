package outputs_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

const instance = "executor-outputs"

type runner struct {
	c   *harness.Clients
	ctx context.Context
}

func start(t *testing.T) runner {
	t.Helper()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	return runner{f.Clients(harness.App), ctx}
}
func (r runner) execute(t *testing.T, cmd *repb.Command, root *repb.Directory) *repb.ExecuteResponse {
	t.Helper()
	if root == nil {
		root = &repb.Directory{}
	}
	rd, err := r.c.UploadProto(r.ctx, instance, root)
	require.NoError(t, err)
	cmd.Platform = &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}
	cd, err := r.c.UploadProto(r.ctx, instance, cmd)
	require.NoError(t, err)
	ad, err := r.c.UploadProto(r.ctx, instance, &repb.Action{CommandDigest: cd, InputRootDigest: rd, DoNotCache: true, Timeout: durationpb.New(time.Minute)})
	require.NoError(t, err)
	stream, err := r.c.Execution.Execute(r.ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: ad, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	for {
		op, err := stream.Recv()
		if err == io.EOF {
			t.Fatal("execution ended without result")
		}
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		if e := op.GetError(); e != nil {
			return &repb.ExecuteResponse{Status: e}
		}
		response := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(response))
		t.Logf("status=%v result=%v", response.GetStatus(), response.GetResult())
		return response
	}
}
func shell(script string) *repb.Command {
	return &repb.Command{Arguments: []string{"/bin/sh", "-c", "set -eu; " + script}}
}
func success(t *testing.T, response *repb.ExecuteResponse) *repb.ActionResult {
	t.Helper()
	require.Zero(t, response.GetStatus().GetCode(), "%v", response.GetStatus())
	require.NotNil(t, response.Result)
	require.Zero(t, response.Result.ExitCode)
	return response.Result
}
func (r runner) read(t *testing.T, d *repb.Digest) []byte {
	t.Helper()
	require.NotNil(t, d)
	b, err := r.c.Read(r.ctx, instance, d)
	require.NoError(t, err)
	require.True(t, proto.Equal(harness.Digest(b), d), "CAS content digest mismatch: got %s, want %s", harness.Digest(b), d)
	return b
}
