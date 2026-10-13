package lifecycle_test

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

var faultSeed = flag.Int64("lifecycle.seed", 6302, "Replayable lifecycle batch seed")

type observation struct {
	name     string
	stage    repb.ExecutionStage_Value
	response *repb.ExecuteResponse
	err      error
	terminal bool
}

// One Execute per action: reconnecting observers must never resubmit work.
func submitHeld(t *testing.T, f *harness.Fixture, ctx context.Context, id string, durableInputs bool) <-chan observation {
	t.Helper()
	c := f.Clients(harness.App)
	root, err := c.UploadProto(ctx, "lifecycle-faults", &repb.Directory{})
	require.NoError(t, err)
	script := fmt.Sprintf("echo attempt >> /data/%s-attempts; touch /data/%s-started; while ! test -f /data/%s-release; do sleep 0.1; done; printf '%s\\n'", id, id, id, id)
	cmd, err := c.UploadProto(ctx, "lifecycle-faults", &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{
		{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}, {Name: "EstimatedCPU", Value: "1000m"},
	}}})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, "lifecycle-faults", &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(180 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	if durableInputs {
		// Pebble uses unsynced writes. Flush inputs through a graceful restart
		// before injecting SIGKILL, so cache durability is not the fault under test.
		f.Restart(harness.App)
		c = f.Clients(harness.App)
		for _, d := range []*repb.Digest{root, cmd, action} {
			_, err := c.Read(ctx, "lifecycle-faults", d)
			require.NoError(t, err, "preflight inputs did not survive graceful restart")
		}
		f.WaitExecutionReady()
	}
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "lifecycle-faults", ActionDigest: action, SkipCacheLookup: true})
	require.NoError(t, err)
	events := make(chan observation, 128)
	go func() {
		defer close(events)
		for {
			op, err := stream.Recv()
			if err != nil {
				events <- observation{err: err}
				return
			}
			o := observation{name: op.Name, terminal: op.Done}
			if op.Done {
				if op.GetError() != nil {
					o.err = status.ErrorProto(op.GetError())
				} else {
					o.response = &repb.ExecuteResponse{}
					o.err = op.GetResponse().UnmarshalTo(o.response)
				}
			} else {
				md := &repb.ExecuteOperationMetadata{}
				o.err = op.GetMetadata().UnmarshalTo(md)
				o.stage = md.Stage
			}
			events <- o
		}
	}()
	return events
}

func waitStage(t *testing.T, events <-chan observation, stage repb.ExecutionStage_Value) string {
	t.Helper()
	for o := range events {
		require.NoError(t, o.err)
		require.False(t, o.terminal, "completed before fault")
		if o.stage == stage {
			require.NotEmpty(t, o.name)
			return o.name
		}
	}
	t.Fatal("stream ended before required stage")
	return ""
}

func waitMarker(t *testing.T, f *harness.Fixture, c harness.Component, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return f.Exec(c, "sh", "-c", "if test -f /data/"+id+"-started; then echo yes; fi") == "yes"
	}, 30*time.Second, 100*time.Millisecond, "command did not start")
}
func release(f *harness.Fixture, c harness.Component, ids ...string) {
	for _, id := range ids {
		f.Exec(c, "touch", "/data/"+id+"-release")
	}
}

func retriable(t *testing.T, err error) {
	t.Helper()
	require.Contains(t, []codes.Code{codes.Unavailable, codes.Aborted, codes.ResourceExhausted}, status.Code(err), "unclear/non-retriable failure: %v", err)
	require.NotEmpty(t, status.Convert(err).Message())
}

// Exactly one terminal response on this observer, or an explicit retriable error.
// This does not promise exactly-once side effects: execution retries are permitted.
func finish(t *testing.T, f *harness.Fixture, ctx context.Context, events <-chan observation, id string, allowTransportFailure bool) {
	t.Helper()
	terminals := 0
	failed := false
	for o := range events {
		if o.terminal {
			terminals++
			require.Equal(t, 1, terminals, "duplicate terminal response")
		}
		if o.err != nil {
			if o.err == io.EOF {
				require.Equal(t, 1, terminals, "EOF before terminal response")
				continue
			}
			require.True(t, allowTransportFailure || o.terminal, "Execute transport failed: %v", o.err)
			retriable(t, o.err)
			failed = true
			continue
		}
		if !o.terminal {
			continue
		}
		resp := o.response
		require.NotNil(t, resp)
		if resp.GetStatus().GetCode() != 0 {
			retriable(t, status.ErrorProto(resp.Status))
			failed = true
			continue
		}
		require.NotNil(t, resp.Result)
		require.Zero(t, resp.Result.ExitCode)
		b := resp.Result.StdoutRaw
		if resp.Result.StdoutDigest != nil {
			var err error
			b, err = f.Clients(harness.App).Read(ctx, "lifecycle-faults", resp.Result.StdoutDigest)
			require.NoError(t, err)
		}
		require.Equal(t, id+"\n", string(b))
	}
	require.True(t, terminals == 1 || failed, "action disappeared without a terminal result or retriable failure")
}

func faultFixture(t *testing.T) *harness.Fixture {
	return harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Args: map[harness.Component][]string{harness.Executor: {"--executor.millicpu=1000", "--max_shutdown_duration=40s", "--shutdown_lameduck_duration=1s"}}})
}

func TestSIGKILLInFlightReschedules(t *testing.T) {
	f := faultFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	id := "kill-" + f.ID
	events := submitHeld(t, f, ctx, id, false)
	waitStage(t, events, repb.ExecutionStage_EXECUTING)
	waitMarker(t, f, harness.Executor, id)
	replacement := f.AddExecutor(harness.Options{})
	// Only the replacement has the release gate, so the killed attempt cannot succeed.
	release(f, replacement, id)
	f.Kill(harness.Executor, "KILL")
	require.Equal(t, 137, f.WaitExit(harness.Executor, true))
	finish(t, f, ctx, events, id, false)
	f.WaitExecutionReady()
}

func TestDrainWithQueuedActions(t *testing.T) {
	f := faultFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	t.Logf("lifecycle.seed=%d", *faultSeed)
	rng := rand.New(rand.NewSource(*faultSeed))
	first := "held-" + f.ID
	running := submitHeld(t, f, ctx, first, false)
	waitStage(t, running, repb.ExecutionStage_EXECUTING)
	waitMarker(t, f, harness.Executor, first)
	ids := []string{"queue-a-" + f.ID, "queue-b-" + f.ID, "queue-c-" + f.ID}
	rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	queued := make([]<-chan observation, len(ids))
	for i, id := range ids {
		queued[i] = submitHeld(t, f, ctx, id, false)
		waitStage(t, queued[i], repb.ExecutionStage_QUEUED)
	}
	// Capacity is fully occupied; verify none of the queued commands actually started.
	for _, id := range ids {
		require.Equal(t, "absent", f.Exec(harness.Executor, "sh", "-c", "if test -f /data/"+id+"-started; then echo started; else echo absent; fi"))
	}
	f.Kill(harness.Executor, "TERM")
	require.Eventually(t, func() bool { return probe(f.Endpoint(harness.Executor), "/readyz", false) == 503 }, 2*time.Second, 50*time.Millisecond)
	release(f, harness.Executor, first)
	finish(t, f, ctx, running, first, false)
	require.Zero(t, f.WaitExit(harness.Executor, false))
	replacement := f.AddExecutor(harness.Options{})
	release(f, replacement, ids...)
	for i, id := range ids {
		finish(t, f, ctx, queued[i], id, false)
	}
	f.WaitExecutionReady()
}

func TestAppOutageWhileExecutorHoldsWork(t *testing.T) {
	f := faultFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	id := "outage-" + f.ID
	events := submitHeld(t, f, ctx, id, true)
	name := waitStage(t, events, repb.ExecutionStage_EXECUTING)
	waitMarker(t, f, harness.Executor, id)
	f.Kill(harness.App, "KILL")
	require.Equal(t, 137, f.WaitExit(harness.App, true))
	// Keep the command blocked through a real outage, then restore the same app data.
	time.Sleep(3 * time.Second)
	require.Equal(t, "1", f.Exec(harness.Executor, "sh", "-c", "wc -l < /data/"+id+"-attempts"))
	f.Start(harness.App)
	release(f, harness.Executor, id)
	finish(t, f, ctx, events, id, true)
	// Independently observe the original operation after reconnecting; never Execute again.
	stream, err := f.Clients(harness.App).Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
	require.NoError(t, err)
	terminals := 0
	for {
		op, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			retriable(t, err)
			break
		}
		if !op.Done {
			continue
		}
		terminals++
		require.Equal(t, 1, terminals)
		require.Nil(t, op.GetError())
		resp := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(resp))
		if resp.GetStatus().GetCode() != 0 {
			retriable(t, status.ErrorProto(resp.Status))
		} else {
			require.NotNil(t, resp.Result)
			require.Zero(t, resp.Result.ExitCode)
		}
	}
	require.Equal(t, 1, terminals, "original operation was not recovered")
	require.Equal(t, "1", f.Exec(harness.Executor, "sh", "-c", "wc -l < /data/"+id+"-attempts"))
	f.WaitExecutionReady()
}
