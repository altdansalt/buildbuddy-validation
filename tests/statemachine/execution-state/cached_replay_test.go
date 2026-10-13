package executionstate_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

// A valid cache hit returns a completed operation name. That name should remain
// usable by the standard WaitExecution API, just like an executed action's name.
func TestCachedExecuteWaitReplay(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	root, err := c.UploadProto(ctx, "", &repb.Directory{})
	require.NoError(t, err)
	command, err := c.UploadProto(ctx, "", &repb.Command{Arguments: []string{"/bin/true"}})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, "", &repb.Action{CommandDigest: command, InputRootDigest: root})
	require.NoError(t, err)
	_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: action, ActionResult: &repb.ActionResult{}})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{ActionDigest: action})
	require.NoError(t, err)
	op, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, op.Done)
	result := &repb.ExecuteResponse{}
	require.NoError(t, op.GetResponse().UnmarshalTo(result))
	require.True(t, result.CachedResult)
	t.Logf("cached operation=%s", op.Name)
	started := time.Now()
	waitCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	replay, err := c.Execution.WaitExecution(waitCtx, &repb.WaitExecutionRequest{Name: op.Name})
	require.NoError(t, err)
	terminal, err := replay.Recv()
	t.Logf("WaitExecution observation after %s: %v", time.Since(started), err)
	require.NoError(t, err, "completed cache-hit Execute operation must be replayable")
	require.True(t, terminal.Done)
}
