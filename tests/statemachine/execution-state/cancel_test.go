package executionstate_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"testing"
	"time"
)

func TestNormallyCancelExecutingAction(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	invocation := uuid.NewString()
	finish := startInvocation(t, ctx, c, invocation)
	defer finish()
	md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: invocation})
	require.NoError(t, err)
	ctx = metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(md))
	root, err := c.UploadProto(ctx, "cancel", &repb.Directory{})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, "cancel", &repb.Command{Arguments: []string{"/bin/sh", "-c", "sleep 300"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, "cancel", &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(10 * time.Minute), DoNotCache: true})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "cancel", ActionDigest: action, SkipCacheLookup: true})
	require.NoError(t, err)
	var name string
	for {
		op, err := stream.Recv()
		require.NoError(t, err)
		require.False(t, op.Done)
		name = op.Name
		stage := &repb.ExecuteOperationMetadata{}
		require.NoError(t, op.Metadata.UnmarshalTo(stage))
		if stage.Stage == repb.ExecutionStage_EXECUTING {
			break
		}
	}
	// BES creates the invocation asynchronously. Poll the authorized read before mutation.
	require.Eventually(t, func() bool {
		_, err := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": invocation}})
		return err == nil
	}, 30*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool {
		res, err := f.Web.RPC("GetExecution", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "executionLookup": map[string]any{"invocationId": invocation, "executionId": name}})
		if err != nil {
			return false
		}
		rows, _ := res["execution"].([]any)
		if len(rows) != 1 {
			return false
		}
		row := rows[0].(map[string]any)
		return row["executionId"] == name && row["stage"] == "EXECUTING"
	}, 30*time.Second, 100*time.Millisecond)
	executingGRPC(t, ctx, c, invocation, name, action)
	_, err = f.Web.RPC("CancelExecutions", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": invocation})
	require.NoError(t, err)
	require.NoError(t, c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/CancelExecutions", envelope(field(2, []byte(invocation))), &emptypb.Empty{}))

	var result *repb.ExecuteResponse
	for {
		op, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, name, op.Name)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		terminal := &repb.ExecuteOperationMetadata{}
		require.NoError(t, op.Metadata.UnmarshalTo(terminal))
		require.Equal(t, repb.ExecutionStage_COMPLETED, terminal.Stage)
		require.True(t, proto.Equal(action, terminal.ActionDigest))
		result = &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(result))
		break
	}
	require.EqualValues(t, 1, result.GetStatus().GetCode(), "normal cancellation must report CANCELLED")
	require.Eventually(t, func() bool {
		res, err := f.Web.RPC("GetExecution", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "executionLookup": map[string]any{"invocationId": invocation, "executionId": name}, "inlineExecuteResponse": true})
		if err != nil {
			return false
		}
		rows, _ := res["execution"].([]any)
		if len(rows) != 1 {
			return false
		}
		row := rows[0].(map[string]any)
		inline, _ := row["executeResponse"].(map[string]any)
		status, _ := inline["status"].(map[string]any)
		return row["executionId"] == name && row["stage"] == "COMPLETED" && status["code"] == float64(1)
	}, 30*time.Second, 100*time.Millisecond)
	replayBB(t, ctx, c, name, result)
	grpcProjection(t, ctx, c, invocation, name, result)
	wait, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
	require.NoError(t, err)
	for {
		op, err := wait.Recv()
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		got := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(got))
		require.True(t, proto.Equal(result, got))
		break
	}
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		_, err := f.Clients(component).AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: "cancel", ActionDigest: action})
		require.Equal(t, codes.NotFound, status.Code(err), "cancelled action must not become cache hit")
	}

}
