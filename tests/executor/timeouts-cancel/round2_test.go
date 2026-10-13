package timeouts_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func waitExecuting(t *testing.T, ctx context.Context, c *harness.Clients, d *repb.Digest) repb.Execution_ExecuteClient {
	t.Helper()
	s, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: true})
	require.NoError(t, err)
	for {
		op, err := s.Recv()
		require.NoError(t, err)
		require.False(t, op.Done)
		md := &repb.ExecuteOperationMetadata{}
		require.NoError(t, op.Metadata.UnmarshalTo(md))
		if md.Stage == repb.ExecutionStage_EXECUTING {
			return s
		}
	}
}
func terminal(t *testing.T, s repb.Execution_ExecuteClient) *repb.ExecuteResponse {
	t.Helper()
	for {
		op, err := s.Recv()
		require.NoError(t, err)
		require.Nil(t, op.GetError())
		if op.Done {
			r := &repb.ExecuteResponse{}
			require.NoError(t, op.GetResponse().UnmarshalTo(r))
			t.Logf("terminal status=%v exit=%d", r.Status, r.GetResult().GetExitCode())
			return r
		}
	}
}
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}

func invocation(t *testing.T, ctx context.Context, c *harness.Clients, id string) {
	t.Helper()
	s, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	started := append(field(1, field(3, nil)), field(5, append(append(field(3, []byte("8.0.0")), field(4, []byte("--bes_upload_mode=wait_for_upload_complete"))...), field(5, []byte("build"))...))...)
	require.NoError(t, s.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: &bespb.StreamId{InvocationId: id, BuildId: id, Component: bespb.StreamId_TOOL}, SequenceNumber: 1, Event: &bespb.BuildEvent{Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: started}}}}}))
	require.NoError(t, s.CloseSend())
	_, err = s.Recv()
	require.NoError(t, err)
}
func cancelAll(t *testing.T, f *harness.Fixture, props ...*repb.Platform_Property) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	id := uuid.NewString()
	invocation(t, ctx, c, id)
	md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: id})
	require.NoError(t, err)
	actionCtx := metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(md))
	streams := []repb.Execution_ExecuteClient{}
	// Unique action digests avoid action-merging cancellation exemptions.
	for i := 0; i < 3; i++ {
		script := fmt.Sprintf("printf action%d; exec sleep 60", i)
		if len(props) == 0 {
			script = fmt.Sprintf("echo $$ > /tmp/%s-cancel-%d; %s", f.ID, i, script)
		}
		d := upload(t, ctx, c, script, 90*time.Second, props...)
		streams = append(streams, waitExecuting(t, actionCtx, c, d))
	}
	// An unrelated action must survive the invocation-wide cancel.
	control := waitExecuting(t, ctx, c, upload(t, ctx, c, "sleep 4; printf unaffected", 15*time.Second, props...))
	start := time.Now()
	_, err = f.Web.RPC("CancelExecutions", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id})
	require.NoError(t, err)
	for _, s := range streams {
		r := terminal(t, s)
		require.Equal(t, int32(codes.Canceled), r.GetStatus().GetCode())
	}
	require.Less(t, time.Since(start), 20*time.Second, "cancel should stop all actions promptly")
	unrelated := terminal(t, control)
	require.Zero(t, unrelated.GetStatus().GetCode())
	require.Zero(t, unrelated.GetResult().GetExitCode())
	require.Equal(t, "unaffected", stdout(t, ctx, c, unrelated))
	if len(props) == 0 {
		// A terminal canceled response alone does not prove the process stopped.
		script := fmt.Sprintf("for f in /tmp/%s-cancel-*; do test -f \"$f\" || continue; pid=$(cat \"$f\"); n=0; while kill -0 \"$pid\" 2>/dev/null && test $n -lt 250; do sleep 0.1; n=$((n+1)); done; rm -f \"$f\"; if kill -0 \"$pid\" 2>/dev/null; then exit 9; fi; done", f.ID)
		probe := execute(t, ctx, c, upload(t, ctx, c, script, 40*time.Second))
		require.Zero(t, probe.GetStatus().GetCode())
		require.Zero(t, probe.GetResult().GetExitCode(), "canceled action process survived")
	} else {
		deadline := time.Now().Add(25 * time.Second)
		var running string
		for {
			b, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "ancestor=mirror.gcr.io/library/alpine:3.21.3", "--format", "{{.ID}}").Output()
			require.NoError(t, err)
			running = strings.TrimSpace(string(b))
			if running == "" || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		require.Empty(t, running, "canceled action containers must stop after lease renewal")
	}
	// Verify the same executor can accept work after cancellation.
	r := execute(t, ctx, c, upload(t, ctx, c, "printf recovered", 10*time.Second, props...))
	require.Zero(t, r.GetStatus().GetCode())
	require.Zero(t, r.GetResult().GetExitCode())
	require.Equal(t, "recovered", stdout(t, ctx, c, r))
}
func TestInvocationCancellation(t *testing.T) {
	f := startFixture(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	cancelAll(t, f)
}
func TestDockerTimeoutCancelAndSignals(t *testing.T) {
	f := dockerFixture(t)
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	props := dockerProps()
	// Warm the image before measuring timeout/cancellation behavior.
	r := execute(t, ctx, c, upload(t, ctx, c, "printf ready", 30*time.Second, props...))
	require.Zero(t, r.GetStatus().GetCode())
	require.Zero(t, r.GetResult().GetExitCode())
	for _, script := range []string{"printf started; exec sleep 30", "trap '' TERM; printf started; exec sleep 30"} {
		r := execute(t, ctx, c, upload(t, ctx, c, script, time.Second, props...))
		require.Equal(t, int32(codes.DeadlineExceeded), r.GetStatus().GetCode())
		require.Equal(t, "started", stdout(t, ctx, c, r))
	}
	cancelAll(t, f, props...)
	// Signal a child shell: Linux protects a container's PID 1 from self-signals.
	for _, tc := range []struct {
		script string
		exit   int32
	}{{"printf signaled; /bin/sh -c 'kill -TERM $$'; code=$?; exit \"$code\"", 143}, {"printf signaled; /bin/sh -c 'kill -KILL $$'; code=$?; exit \"$code\"", 137}, {"printf signaled; exit 137", 137}} {
		r := execute(t, ctx, c, upload(t, ctx, c, tc.script, 10*time.Second, props...))
		require.Zero(t, r.GetStatus().GetCode())
		require.Equal(t, tc.exit, r.GetResult().GetExitCode())
		require.Equal(t, "signaled", stdout(t, ctx, c, r))
	}
	t.Run("real_cgroup_oom", func(t *testing.T) {
		// Limit only this fixture's action container, never the runner or executor.
		// The delay gives the test time to apply the cgroup limit before allocation.
		eventsCtx, stop := context.WithCancel(ctx)
		defer stop()
		events := exec.CommandContext(eventsCtx, "docker", "events", "--filter", "event=oom", "--format", "{{.Actor.ID}}")
		var eventLog strings.Builder
		events.Stdout = &eventLog
		require.NoError(t, events.Start())
		s := waitExecuting(t, ctx, c, upload(t, ctx, c, "printf allocating; sleep 8; exec awk 'BEGIN {s=\"xxxxxxxxxxxxxxxx\"; while (1) s=s s}'", 30*time.Second, props...))
		var container string
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			b, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "ancestor=mirror.gcr.io/library/alpine:3.21.3", "--format", "{{.ID}}").Output()
			require.NoError(t, err)
			container = strings.TrimSpace(string(b))
			if container != "" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		require.NotEmpty(t, container)
		require.NotContains(t, container, "\n", "exactly one test-owned action must be running")
		b, err := exec.CommandContext(ctx, "docker", "update", "--memory", "64m", "--memory-swap", "64m", container).CombinedOutput()
		require.NoError(t, err, string(b))
		r := terminal(t, s)
		stop()
		events.Wait()
		require.Contains(t, eventLog.String(), container, "daemon must independently confirm a cgroup OOM")
		require.Equal(t, int32(137), r.GetResult().GetExitCode())
		require.Zero(t, r.GetStatus().GetCode())
		require.Equal(t, "allocating", stdout(t, ctx, c, r))
		t.Logf("confirmed Docker OOM event: %s", eventLog.String())
	})
}
