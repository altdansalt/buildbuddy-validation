package timeouts_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTimeoutsAndCancellation(t *testing.T) {
	f := startFixture(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Args: map[harness.Component][]string{harness.Executor: {"--executor.max_task_timeout=15s", "--executor.default_task_timeout=2s"}}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	t.Run("deadline_and_partial_stdout", func(t *testing.T) {
		r := execute(t, ctx, c, upload(t, ctx, c, "printf started; exec sleep 30", time.Second))
		require.Equal(t, int32(codes.DeadlineExceeded), r.GetStatus().GetCode())
		require.NotNil(t, r.Result)
		require.Less(t, r.Result.ExitCode, int32(0))
		require.Equal(t, "started", stdout(t, ctx, c, r))
		require.NotNil(t, r.Result.ExecutionMetadata)
		elapsed := r.Result.ExecutionMetadata.ExecutionCompletedTimestamp.AsTime().Sub(r.Result.ExecutionMetadata.ExecutionStartTimestamp.AsTime())
		require.InDelta(t, 1, elapsed.Seconds(), 2, "timeout must bound execution, not queueing")
	})
	t.Run("omitted_timeout_uses_executor_default", func(t *testing.T) {
		r := execute(t, ctx, c, upload(t, ctx, c, "printf default; exec sleep 30", 0))
		require.Equal(t, int32(codes.DeadlineExceeded), r.GetStatus().GetCode())
		require.Equal(t, "default", stdout(t, ctx, c, r))
	})
	t.Run("force_kill_after_ignored_term", func(t *testing.T) {
		r := execute(t, ctx, c, upload(t, ctx, c, "trap '' TERM; printf ignoring; exec sleep 30", time.Second, &repb.Platform_Property{Name: "termination-grace-period", Value: "1s"}))
		require.Equal(t, int32(codes.DeadlineExceeded), r.GetStatus().GetCode())
		require.Less(t, r.GetResult().GetExitCode(), int32(0))
		require.Equal(t, "ignoring", stdout(t, ctx, c, r))
		elapsed := r.Result.ExecutionMetadata.ExecutionCompletedTimestamp.AsTime().Sub(r.Result.ExecutionMetadata.ExecutionStartTimestamp.AsTime())
		require.InDelta(t, 2, elapsed.Seconds(), 2)
	})
	t.Run("self_signal_reports_killed_action", func(t *testing.T) {
		r := execute(t, ctx, c, upload(t, ctx, c, "printf signaled; kill -TERM $$", 5*time.Second))
		require.NotZero(t, r.GetStatus().GetCode(), "killed action must not report success")
		require.Less(t, r.GetResult().GetExitCode(), int32(0))
		require.NotEqual(t, int32(codes.DeadlineExceeded), r.GetStatus().GetCode(), "self-signal did not exhaust timeout")
		require.Equal(t, "signaled", stdout(t, ctx, c, r))
	})

	t.Run("nonzero_exit_is_not_rpc_failure", func(t *testing.T) {
		r := execute(t, ctx, c, upload(t, ctx, c, "printf intentional; exit 42", 5*time.Second))
		require.Zero(t, r.GetStatus().GetCode())
		require.Equal(t, int32(42), r.GetResult().GetExitCode())
		require.Equal(t, "intentional", stdout(t, ctx, c, r))
	})
	t.Run("maximum_timeout_rejected", func(t *testing.T) {
		r := execute(t, ctx, c, upload(t, ctx, c, "printf must-not-run", 20*time.Second))
		require.Equal(t, int32(codes.InvalidArgument), r.GetStatus().GetCode())
		require.Empty(t, stdout(t, ctx, c, r))
	})
	t.Run("graceful_timeout_remains_deadline_even_if_trap_exits_zero", func(t *testing.T) {
		r := execute(t, ctx, c, upload(t, ctx, c, "trap 'printf terminated; exit 0' TERM; printf started; while :; do sleep 0.1; done", time.Second, &repb.Platform_Property{Name: "termination-grace-period", Value: "2s"}))
		require.Equal(t, int32(codes.DeadlineExceeded), r.GetStatus().GetCode())
		require.Less(t, r.GetResult().GetExitCode(), int32(0))
		require.Equal(t, "startedterminated", stdout(t, ctx, c, r))
	})
	t.Run("timeout_kills_descendants", func(t *testing.T) {
		marker := fmt.Sprintf("/tmp/timeout-child-%s", f.ID)
		r := execute(t, ctx, c, upload(t, ctx, c, fmt.Sprintf("(trap '' TERM; sleep 5; printf survived > %s) & printf started; wait", marker), time.Second))
		require.Equal(t, int32(codes.DeadlineExceeded), r.GetStatus().GetCode())
		require.Equal(t, "started", stdout(t, ctx, c, r))
		// Probe inside the same fixture-owned executor, after the child's intended write.
		probe := execute(t, ctx, c, upload(t, ctx, c, fmt.Sprintf("sleep 6; if test -e %s; then cat %s; rm -f %s; exit 9; fi; printf clean", marker, marker, marker), 10*time.Second))
		require.Zero(t, probe.GetStatus().GetCode())
		require.Zero(t, probe.GetResult().GetExitCode(), "timed-out descendant survived: %s", stdout(t, ctx, c, probe))
		require.Equal(t, "clean", stdout(t, ctx, c, probe))
	})
	for _, disconnect := range []bool{false, true} {
		name := "cancel_execute_stream"
		if disconnect {
			name = "disconnect_connection"
		}
		t.Run(name, func(t *testing.T) {
			client := f.Clients(harness.App)
			streamCtx, stop := context.WithCancel(ctx)
			defer stop()
			d := upload(t, ctx, c, "sleep 4; printf completed", 10*time.Second)
			s, err := client.Execution.Execute(streamCtx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: true})
			require.NoError(t, err)
			var operationName string
			for {
				op, err := s.Recv()
				require.NoError(t, err)
				require.False(t, op.Done, "action completed before disconnect")
				md := &repb.ExecuteOperationMetadata{}
				require.NoError(t, op.GetMetadata().UnmarshalTo(md))
				if md.Stage == repb.ExecutionStage_EXECUTING {
					operationName = op.Name
					break
				}
			}
			if disconnect {
				require.NoError(t, client.Conn.Close())
			} else {
				stop()
			}
			// gRPC may deliver updates buffered before cancellation. Drain them
			// until the canceled stream returns its terminal error.
			for err == nil {
				_, err = s.Recv()
			}
			require.Error(t, err)
			if !disconnect {
				require.Equal(t, codes.Canceled, status.Code(err))
			}
			// Canceling an RPC subscription does not call Operations.CancelOperation.
			wait, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: operationName})
			require.NoError(t, err)
			for {
				op, err := wait.Recv()
				require.NoError(t, err, "disconnected execution must remain observable")
				require.Nil(t, op.GetError())
				if !op.Done {
					continue
				}
				r := &repb.ExecuteResponse{}
				require.NoError(t, op.GetResponse().UnmarshalTo(r))
				require.Zero(t, r.GetStatus().GetCode())
				require.Zero(t, r.GetResult().GetExitCode())
				require.Equal(t, "completed", stdout(t, ctx, c, r))
				break
			}
		})
	}
}
