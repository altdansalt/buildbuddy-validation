package executionstate_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestExecutionPublicationAndCacheAgreement(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.True(t, caps.GetExecutionCapabilities().GetExecEnabled())
	instance := ""
	invocation := uuid.NewString()
	finishInvocation := startInvocation(t, ctx, c, invocation)
	defer finishInvocation()
	md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: invocation})
	require.NoError(t, err)
	ctx = metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(md))
	input, err := c.Upload(ctx, instance, []byte("hello from a released executor\n"))
	require.NoError(t, err)
	root, err := c.UploadProto(ctx, instance, &repb.Directory{Files: []*repb.FileNode{{Name: "input.txt", Digest: input}}})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{
		Arguments:   []string{"/bin/sh", "-c", "cat input.txt > output.txt; printf 'done\\n'"},
		OutputPaths: []string{"output.txt"},
		Platform:    &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}},
	})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second), DoNotCache: false})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	var result *repb.ExecuteResponse
	var name string
	var previous repb.ExecutionStage_Value
	seen := map[repb.ExecutionStage_Value]bool{}
	for {
		op, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if name == "" {
			name = op.Name
		}
		require.Equal(t, name, op.Name)
		stage := &repb.ExecuteOperationMetadata{}
		require.NoError(t, op.Metadata.UnmarshalTo(stage))
		require.GreaterOrEqual(t, int(stage.Stage), int(previous))
		previous = stage.Stage
		seen[stage.Stage] = true
		require.True(t, proto.Equal(action, stage.ActionDigest))
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError(), "operation %s failed", op.Name)
		result = &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(result))
		break
	}
	require.NotNil(t, result, "stream ended without a completed operation")
	require.Zero(t, result.GetStatus().GetCode(), "%v", result.GetStatus())
	require.Zero(t, result.GetResult().GetExitCode())
	require.Len(t, result.GetResult().GetOutputFiles(), 1)
	require.Equal(t, repb.ExecutionStage_COMPLETED, previous)
	require.True(t, seen[repb.ExecutionStage_QUEUED], "submission must expose queued state")
	require.True(t, seen[repb.ExecutionStage_EXECUTING], "executor must publish executing state")
	out := result.Result.OutputFiles[0]
	require.Equal(t, "output.txt", out.Path)
	b, err := c.Read(ctx, instance, out.Digest)
	require.NoError(t, err)
	require.Equal(t, []byte("hello from a released executor\n"), b)
	if result.Result.StdoutDigest != nil {
		b, err = c.Read(ctx, instance, result.Result.StdoutDigest)
		require.NoError(t, err)
	} else {
		b = result.Result.StdoutRaw
	}
	require.Equal(t, []byte("done\n"), b)
	grpcProjection(t, ctx, c, invocation, name, result)
	replayBB(t, ctx, c, name, result)
	for replay := 0; replay < 2; replay++ {
		wait, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
		require.NoError(t, err)
		for {
			op, err := wait.Recv()
			require.NoError(t, err)
			require.Equal(t, name, op.Name)
			if !op.Done {
				continue
			}
			got := &repb.ExecuteResponse{}
			require.NoError(t, op.GetResponse().UnmarshalTo(got))
			require.True(t, proto.Equal(result, got), "terminal replay changed result")
			break
		}
	}
	require.Eventually(t, func() bool {
		response, err := f.Web.RPC("GetExecution", map[string]any{
			"requestContext":        f.Web.RequestContext(f.GroupID),
			"executionLookup":       map[string]any{"invocationId": invocation, "executionId": name},
			"inlineExecuteResponse": true,
		})
		if err != nil {
			t.Log(err)
			return false
		}
		executions, _ := response["execution"].([]any)
		if len(executions) != 1 {
			return false
		}
		ex := executions[0].(map[string]any)
		if ex["executionId"] != name {
			return false
		}
		stage := ex["stage"]
		if stage != float64(4) && stage != "COMPLETED" {
			return false
		}
		inline, _ := ex["executeResponse"].(map[string]any)
		ar, _ := inline["result"].(map[string]any)
		files, _ := ar["outputFiles"].([]any)
		if len(files) != 1 {
			return false
		}
		file := files[0].(map[string]any)
		digest, _ := file["digest"].(map[string]any)
		return file["path"] == out.Path && digest["hash"] == out.Digest.Hash
	}, 30*time.Second, 100*time.Millisecond, "UI execution projection must converge")
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		surface := f.Clients(component)
		require.Eventually(t, func() bool {
			cached, err := surface.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
			return err == nil && responseEqual(&repb.ExecuteResponse{Result: result.Result}, &repb.ExecuteResponse{Result: cached})
		}, 30*time.Second, 100*time.Millisecond, "action cache must match published result: %s", component)
		bytes, err := surface.Read(ctx, instance, out.Digest)
		require.NoError(t, err)
		require.Equal(t, []byte("hello from a released executor\n"), bytes)
		missing, err := surface.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{out.Digest}})
		require.NoError(t, err)
		require.Empty(t, missing.MissingBlobDigests)
	}

	cachedStream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action})
	require.NoError(t, err)
	for {
		op, err := cachedStream.Recv()
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		cached := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(cached))
		require.True(t, cached.CachedResult)
		require.True(t, responseEqual(&repb.ExecuteResponse{Result: result.Result}, &repb.ExecuteResponse{Result: cached.Result}))
		break
	}

	downloadCLI(t, ctx, f, out.Digest, []byte("hello from a released executor\n"))

}
