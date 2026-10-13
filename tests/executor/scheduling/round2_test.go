package scheduling_test

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

func submit(t *testing.T, ctx context.Context, c *harness.Clients, d *repb.Digest, priority int32) (string, <-chan observation) {
	t.Helper()
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: true, ExecutionPolicy: &repb.ExecutionPolicy{Priority: priority}})
	require.NoError(t, err)
	first, err := stream.Recv()
	require.NoError(t, err)
	require.False(t, first.Done)
	ch := make(chan observation, 1)
	go func() { ch <- receive(stream) }()
	return first.Name, ch
}

func marker(t *testing.T, f *harness.Fixture, executor harness.Component, path string) {
	t.Helper()
	require.Eventually(t, func() bool { return f.Exec(executor, "sh", "-c", "if test -f "+path+"; then echo yes; fi") == "yes" }, 30*time.Second, 100*time.Millisecond, "command did not start on %s", executor)
}

// Hold the only slot until every reservation is in the executor's local queue.
// EXECUTING is published before shell startup, so use a shell-created marker.
func TestSeededPriorityAndStrictGroupFairness(t *testing.T) {
	for _, fairness := range []bool{false, true} {
		t.Run(fmt.Sprintf("fairness=%t", fairness), func(t *testing.T) {
			f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"max_concurrent_tasks": 1}}}, Args: map[harness.Component][]string{harness.Executor: {"--app.log_level=debug"}}})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			c := f.Clients(harness.App)
			clients := []*harness.Clients{c}
			if fairness {
				for i := 1; i < 3; i++ {
					group := f.Web.CreateGroup(t, fmt.Sprintf("scheduler-%d", i))
					key := f.Web.CreateAPIKey(t, group, "scheduling", []string{"CACHE_WRITE"})
					clients = append(clients, harness.Dial(t, f.Endpoint(harness.App).GRPC, key))
				}
			}
			started, release := "/tmp/"+f.ID+"-started", "/tmp/"+f.ID+"-release"
			_, blocker := submit(t, ctx, c, action(t, ctx, c, fmt.Sprintf("touch %s; while ! test -f %s; do sleep .1; done; printf blocker", started, release), false), 0)
			marker(t, f, harness.Executor, started)
			const seed int64 = 20261013
			t.Logf("seed=%d", seed)
			rng := rand.New(rand.NewSource(seed))
			type task struct {
				group    int
				priority int32
				label    string
				result   <-chan observation
				start    time.Time
			}
			var tasks []*task
			for g := range clients {
				for j := 0; j < 5; j++ {
					tasks = append(tasks, &task{group: g, priority: int32(j*7 - 14), label: fmt.Sprintf("g%d-task%d", g, j)})
				}
			}
			rng.Shuffle(len(tasks), func(i, j int) { tasks[i], tasks[j] = tasks[j], tasks[i] })
			for _, task := range tasks {
				client := clients[task.group]
				name, ch := submit(t, ctx, client, action(t, ctx, client, "sleep .1; printf "+task.label, false), task.priority)
				task.result = ch
				require.Eventually(t, func() bool {
					return strings.Contains(f.Logs(harness.Executor), fmt.Sprintf("Added task %q to pq.", name))
				}, 30*time.Second, 100*time.Millisecond, "reservation not queued: %s", name)
			}
			f.Exec(harness.Executor, "touch", release)
			check(t, ctx, c, <-blocker, "blocker", 0)
			for _, task := range tasks {
				o := <-task.result
				check(t, ctx, clients[task.group], o, task.label, 0)
				require.NotNil(t, o.response.Result.ExecutionMetadata.GetExecutionStartTimestamp())
				task.start = o.response.Result.ExecutionMetadata.ExecutionStartTimestamp.AsTime()
			}
			sort.Slice(tasks, func(i, j int) bool { return tasks[i].start.Before(tasks[j].start) })
			last := map[int]int32{}
			counts := make([]int, len(clients))
			for _, task := range tasks {
				if prev, ok := last[task.group]; ok {
					require.Greater(t, task.priority, prev, "priority inversion within group")
				}
				last[task.group] = task.priority
				counts[task.group]++
				if fairness {
					lo, hi := counts[0], counts[0]
					for _, v := range counts {
						if v < lo {
							lo = v
						}
						if v > hi {
							hi = v
						}
					}
					require.LessOrEqual(t, hi-lo, 1, "non-round-robin prefix: counts=%v", counts)
				}
			}
		})
	}
}

func TestAbruptKillsRetryUntilExhaustion(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Args: map[harness.Component][]string{harness.App: {"--remote_execution.lease_duration=2s", "--remote_execution.lease_grace_period=2s", "--remote_execution.lease_reconnect_grace_period=0s"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	path := "/tmp/" + f.ID + "-attempt"
	name, result := submit(t, ctx, c, action(t, ctx, c, "touch "+path+"; sleep 40; printf unexpected-success", false), 0)
	current := harness.Executor
	var o observation
	kills := 0
attempts:
	for attempt := 1; attempt <= 6; attempt++ {
		deadline := time.NewTimer(45 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		started := false
		for !started {
			select {
			case o = <-result:
				deadline.Stop()
				ticker.Stop()
				break attempts
			case <-deadline.C:
				ticker.Stop()
				t.Fatalf("no command start or terminal response on attempt %d; app logs: %s", attempt, f.Logs(harness.App))
			case <-ticker.C:
				started = f.Exec(current, "sh", "-c", "if test -f "+path+"; then echo yes; fi") == "yes"
			}
		}
		deadline.Stop()
		ticker.Stop()
		require.LessOrEqual(t, attempt, 5, "scheduler exceeded its five-claim retry budget")
		t.Logf("operation=%s killing command start=%d executor=%s", name, attempt, current)
		f.Kill(current, "KILL")
		require.NotZero(t, f.WaitExit(current, true))
		kills++
		// Claims count towards the retry budget before shell startup; a killed
		// executor may claim a retry during the fault-injection race. Watch the
		// terminal response as well as the fresh container's marker.
		current = f.AddExecutor(harness.Options{})
	}
	require.GreaterOrEqual(t, kills, 2, "must exercise repeated abrupt failures")
	require.NoError(t, o.err)
	require.Equal(t, name, o.name)
	require.EqualValues(t, 13, o.response.GetStatus().GetCode(), "%v", o.response)
	require.Contains(t, o.response.GetStatus().GetMessage(), "already attempted 5 times")
	survivor := f.AddExecutor(harness.Options{})
	require.NotEmpty(t, survivor)
	check(t, ctx, c, execute(ctx, c, action(t, ctx, c, "printf healthy-after-exhaustion", false)), "healthy-after-exhaustion", 0)
}

func TestConfiguredHedgeRunsOneAdditionalAttempt(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"max_concurrent_tasks": 2}}}, Args: map[harness.Component][]string{harness.App: {"--remote_execution.action_merging_hedge_count=1", "--remote_execution.action_merging_hedge_delay=0s"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	counter, release := "/tmp/"+f.ID+"-hedges", "/tmp/"+f.ID+"-release"
	d := action(t, ctx, c, fmt.Sprintf("echo attempt >> %s; while ! test -f %s; do sleep .1; done; printf hedged", counter, release), true)
	name, first := submit(t, ctx, c, d, 0)
	marker(t, f, harness.Executor, counter)
	var results []<-chan observation
	for i := 0; i < 6; i++ {
		n, ch := submit(t, ctx, c, d, 0)
		require.Equal(t, name, n)
		results = append(results, ch)
	}
	require.Eventually(t, func() bool { return f.Exec(harness.Executor, "sh", "-c", "wc -l < "+counter) == "2" }, 30*time.Second, 100*time.Millisecond, "hedged command never started")
	f.Exec(harness.Executor, "touch", release)
	check(t, ctx, c, <-first, "hedged", 0)
	for _, ch := range results {
		check(t, ctx, c, <-ch, "hedged", 0)
	}
	require.Equal(t, "2", f.Exec(harness.Executor, "sh", "-c", "wc -l < "+counter), "hedge budget exceeded")
}
