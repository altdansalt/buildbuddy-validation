package scheduling_test

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

const instance = "scheduling"

// See v2.314.0 enterprise/server/scheduling/priority_task_scheduler and
// enterprise/server/remote_execution/action_merger. REAPI Execute permits
// redundant execution, while WaitExecution must allow attaching to an operation.
func action(t *testing.T, ctx context.Context, c *harness.Clients, script string, cacheable bool) *repb.Digest {
	t.Helper()
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{
		Arguments: []string{"/bin/sh", "-c", script},
		Platform: &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"},
		}},
	})
	require.NoError(t, err)
	d, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(45 * time.Second), DoNotCache: !cacheable})
	require.NoError(t, err)
	return d
}

type observation struct {
	name     string
	response *repb.ExecuteResponse
	err      error
}

func execute(ctx context.Context, c *harness.Clients, d *repb.Digest) observation {
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	if err != nil {
		return observation{err: err}
	}
	return receive(stream)
}

func receive(stream repb.Execution_ExecuteClient) observation {
	var o observation
	for {
		op, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				err = fmt.Errorf("EOF before terminal operation")
			}
			o.err = err
			return o
		}
		if o.name != "" && o.name != op.Name {
			o.err = fmt.Errorf("operation changed from %s to %s", o.name, op.Name)
			return o
		}
		o.name = op.Name
		if !op.Done {
			continue
		}
		if op.GetError() != nil {
			o.err = fmt.Errorf("operation error: %v", op.GetError())
			return o
		}
		o.response = &repb.ExecuteResponse{}
		o.err = op.GetResponse().UnmarshalTo(o.response)
		return o
	}
}

func check(t *testing.T, ctx context.Context, c *harness.Clients, o observation, expected string, exit int32) {
	t.Helper()
	require.NoError(t, o.err)
	require.NotEmpty(t, o.name)
	require.Zero(t, o.response.GetStatus().GetCode(), "%v", o.response.GetStatus())
	require.NotNil(t, o.response.Result)
	require.Equal(t, exit, o.response.Result.ExitCode)
	data := o.response.Result.StdoutRaw
	if o.response.Result.StdoutDigest != nil {
		var err error
		data, err = c.Read(ctx, instance, o.response.Result.StdoutDigest)
		require.NoError(t, err)
	}
	require.Equal(t, expected, string(data))
}

func TestQueueConcurrencyAndFailureIsolation(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"max_concurrent_tasks": 2}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	const n = 16
	actions := make([]*repb.Digest, n)
	for i := range actions {
		actions[i] = action(t, ctx, c, fmt.Sprintf("sleep 1; printf 'task-%d'; exit %d", i, i%3), false)
	}
	results := make([]observation, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range actions {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; results[i] = execute(ctx, c, actions[i]) }(i)
	}
	close(start)
	wg.Wait()
	type event struct {
		at    time.Time
		delta int
	}
	var events []event
	names := map[string]bool{}
	for i, o := range results {
		check(t, ctx, c, o, fmt.Sprintf("task-%d", i), int32(i%3))
		require.False(t, names[o.name], "distinct actions must have distinct operations")
		names[o.name] = true
		m := o.response.Result.ExecutionMetadata
		require.NotNil(t, m)
		require.NotNil(t, m.ExecutionStartTimestamp)
		require.NotNil(t, m.ExecutionCompletedTimestamp)
		begin, end := m.ExecutionStartTimestamp.AsTime(), m.ExecutionCompletedTimestamp.AsTime()
		require.True(t, end.After(begin))
		events = append(events, event{begin, 1}, event{end, -1})
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at.Equal(events[j].at) {
			return events[i].delta < events[j].delta
		}
		return events[i].at.Before(events[j].at)
	})
	running, peak := 0, 0
	for _, e := range events {
		running += e.delta
		if running > peak {
			peak = running
		}
	}
	require.LessOrEqual(t, peak, 2, "execution intervals exceed configured concurrency")
	require.Equal(t, 2, peak, "batch should exercise parallel execution and queueing")
	check(t, ctx, c, execute(ctx, c, action(t, ctx, c, "printf healthy", false)), "healthy", 0)
}

func TestDuplicateInFlightAndWaitAfterDisconnect(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	d := action(t, ctx, c, "sleep 8; printf merged", true)
	clientCtx, disconnect := context.WithCancel(ctx)
	stream, err := c.Execution.Execute(clientCtx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: true})
	require.NoError(t, err)
	first, err := stream.Recv()
	require.NoError(t, err)
	require.False(t, first.Done)
	// Attach duplicates while the original is in flight, before dropping its stream.
	const n = 6
	results := make(chan observation, n)
	for i := 0; i < n; i++ {
		go func() { results <- execute(ctx, c, d) }()
	}
	wait, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: first.Name})
	require.NoError(t, err)
	disconnect()
	attached := receive(wait)
	check(t, ctx, c, attached, "merged", 0)
	require.Equal(t, first.Name, attached.name)
	for i := 0; i < n; i++ {
		o := <-results
		check(t, ctx, c, o, "merged", 0)
		require.Equal(t, first.Name, o.name, "default action merging should reuse in-flight operation")
	}
	// A completed operation remains attachable and returns the same result.
	wait, err = c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: first.Name})
	require.NoError(t, err)
	check(t, ctx, c, receive(wait), "merged", 0)
}

func TestExecutorShutdownRetriesOnAnotherExecutor(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor},
		Args:       map[harness.Component][]string{harness.Executor: {"--max_shutdown_duration=5s", "--executor.shutdown_cleanup_duration=1s"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	d := action(t, ctx, c, "sleep 20; printf recovered", false)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: true})
	require.NoError(t, err)
	// Observe actual execution before shutting down, rather than racing registration.
	var name string
	for {
		op, err := stream.Recv()
		require.NoError(t, err)
		require.False(t, op.Done, "action completed before fault injection")
		name = op.Name
		m := &repb.ExecuteOperationMetadata{}
		require.NoError(t, op.Metadata.UnmarshalTo(m))
		if m.Stage == repb.ExecutionStage_EXECUTING {
			break
		}
	}
	second := f.AddExecutor(harness.Options{})
	require.NotEqual(t, f.Endpoint(harness.Executor).Container, f.Endpoint(second).Container)
	f.Stop(harness.Executor)
	result := receive(stream)
	check(t, ctx, c, result, "recovered", 0)
	require.Equal(t, name, result.name, "retry must preserve the operation")
	// Only the surviving executor can serve this newly submitted distinct action.
	check(t, ctx, c, execute(ctx, c, action(t, ctx, c, "printf survivor", false)), "survivor", 0)
}
