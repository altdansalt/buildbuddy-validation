package execution_test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func executeRequest(t *testing.T, c *harness.Clients, ctx context.Context, req *repb.ExecuteRequest) *repb.ExecuteResponse {
	t.Helper()
	s, err := c.Execution.Execute(ctx, req)
	require.NoError(t, err)
	first, err := s.Recv()
	require.NoError(t, err)
	if first.Done {
		r := &repb.ExecuteResponse{}
		require.NoError(t, first.GetResponse().UnmarshalTo(r))
		require.Zero(t, r.GetStatus().GetCode())
		return r
	}
	return finish(t, s, first.Name, req.ActionDigest)
}

func TestPolicyMatrixAndInflightMerging(t *testing.T) {
	c, ctx := fixture(t, true)
	rng := rand.New(rand.NewSource(20261013))
	for _, noCache := range []bool{false, true} {
		d := uploadAction(t, c, ctx, fmt.Sprintf("printf matrix-%v", noCache), noCache)
		for i, index := range rng.Perm(6) {
			skip := index%2 == 1
			priority := []int32{-1000, 0, 1000}[index/2]
			r := executeRequest(t, c, ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, SkipCacheLookup: skip, ResultsCachePolicy: &repb.ResultsCachePolicy{Priority: priority}})
			require.Equal(t, !noCache && !skip && i > 0, r.CachedResult, "seed=20261013 noCache=%v step=%d skip=%v priority=%d", noCache, i, skip, priority)
			require.Zero(t, r.Result.ExitCode)
		}
	}
	for _, noCache := range []bool{false, true} {
		d := uploadAction(t, c, ctx, fmt.Sprintf("sleep 5; printf merge-%v", noCache), noCache)
		streams := make([]repb.Execution_ExecuteClient, 3)
		names := make([]string, 3)
		for i := range streams {
			var err error
			streams[i], err = c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, SkipCacheLookup: i%2 == 0, ResultsCachePolicy: &repb.ResultsCachePolicy{Priority: int32(i - 1)}})
			require.NoError(t, err)
			first, err := streams[i].Recv()
			require.NoError(t, err)
			require.False(t, first.Done)
			names[i] = first.Name
		}
		for i, s := range streams {
			r := finish(t, s, names[i], d)
			require.False(t, r.CachedResult)
			if !noCache {
				require.Equal(t, names[0], names[i], "cacheable in-flight actions should merge")
			}
		}
		if noCache {
			require.NotEqual(t, names[0], names[1])
			require.NotEqual(t, names[1], names[2])
			require.NotEqual(t, names[0], names[2])
		}
	}
}

// Locate only the daemon which owns this fixture, including the harness's private
// daemon fallback. Every subsequent command is restricted to fixture containers.
func dockerFixture(t *testing.T, f *harness.Fixture, args ...string) string {
	t.Helper()
	sockets, _ := filepath.Glob("/tmp/bbval-docker-*/docker.sock")
	hosts := []string{os.Getenv("DOCKER_HOST")}
	for _, s := range sockets {
		hosts = append(hosts, "unix://"+s)
	}
	for _, host := range hosts {
		prefix := []string{}
		if host != "" {
			prefix = append(prefix, "--host", host)
		}
		probe := exec.Command("docker", append(append([]string{}, prefix...), "inspect", f.Endpoint(harness.App).Container)...)
		if probe.Run() != nil {
			continue
		}
		out, err := exec.Command("docker", append(prefix, args...)...).CombinedOutput()
		require.NoError(t, err, "docker: %s", out)
		return string(out)
	}
	t.Fatal("fixture Docker daemon not found")
	return ""
}

func TestWaitUnknownAndExpiredMonitoredOperations(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Args: map[harness.Component][]string{harness.App: {"--remote_execution.enable_redis_availability_monitoring=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	d := uploadAction(t, c, ctx, "printf replay-before-expiry", false)
	s, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, SkipCacheLookup: true})
	require.NoError(t, err)
	first, err := s.Recv()
	require.NoError(t, err)
	require.False(t, first.Done)
	finish(t, s, first.Name, d)
	replay, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: first.Name})
	require.NoError(t, err)
	finish(t, replay, first.Name, d)
	key := "monitoredPubSub/taskStatusStream/{" + first.Name + "}"
	redis := f.ID + "-redis"
	require.Equal(t, "1", strings.TrimSpace(dockerFixture(t, f, "exec", redis, "redis-cli", "EXISTS", key)))
	require.Equal(t, "1", strings.TrimSpace(dockerFixture(t, f, "exec", redis, "redis-cli", "PEXPIRE", key, "1")))
	require.Eventually(t, func() bool {
		return strings.TrimSpace(dockerFixture(t, f, "exec", redis, "redis-cli", "EXISTS", key)) == "0"
	}, 5*time.Second, 50*time.Millisecond)
	unknown := harness.UploadResourceName("streams", d, false)
	for _, name := range []string{unknown, first.Name} {
		waitCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		w, err := c.Execution.WaitExecution(waitCtx, &repb.WaitExecutionRequest{Name: name})
		if err == nil {
			op, recvErr := w.Recv()
			err = recvErr
			if err == nil {
				require.True(t, op.Done)
				r := &repb.ExecuteResponse{}
				require.NoError(t, op.GetResponse().UnmarshalTo(r))
				require.EqualValues(t, codes.NotFound, r.GetStatus().GetCode())
				_, err = w.Recv()
				require.Equal(t, io.EOF, err)
				err = status.Error(codes.NotFound, "terminal operation reports missing stream")
			}
		}
		stop()
		require.Equal(t, codes.NotFound, status.Code(err), "name=%s err=%v", name, err)
	}
}

func TestSchedulerPriorityOrdering(t *testing.T) {
	gate := t.TempDir()
	require.NoError(t, os.Chmod(gate, 0777))
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Args: map[harness.Component][]string{harness.Executor: {"--executor.max_concurrent_tasks=1", "--app.log_level=debug"}}, DockerArgs: map[harness.Component][]string{harness.Executor: {"--volume", gate + ":/priority-gate"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	blocker := uploadAction(t, c, ctx, "touch /priority-gate/started; while [ ! -f /priority-gate/release ]; do sleep 0.1; done", true)
	b, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: blocker})
	require.NoError(t, err)
	initial, err := b.Recv()
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(gate, "started")); return err == nil }, 20*time.Second, 100*time.Millisecond)
	priorities := []int32{500, -500, 0}
	rng := rand.New(rand.NewSource(20261013))
	rng.Shuffle(len(priorities), func(i, j int) { priorities[i], priorities[j] = priorities[j], priorities[i] })
	type pending struct {
		p    int32
		d    *repb.Digest
		name string
		s    repb.Execution_ExecuteClient
	}
	var queued []pending
	for _, p := range priorities {
		d := uploadAction(t, c, ctx, fmt.Sprintf("printf '%%s\\n' '%d' >> /priority-gate/order", p), true)
		s, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, ExecutionPolicy: &repb.ExecutionPolicy{Priority: p}})
		require.NoError(t, err)
		op, err := s.Recv()
		require.NoError(t, err)
		require.False(t, op.Done)
		queued = append(queued, pending{p, d, op.Name, s})
	}
	// Confirm reservations reached the executor queue before releasing the slot.
	require.Eventually(t, func() bool {
		logs := dockerFixture(t, f, "logs", f.Endpoint(harness.Executor).Container)
		for _, q := range queued {
			if !strings.Contains(logs, fmt.Sprintf("Added task %q to pq.", q.name)) {
				return false
			}
		}
		return true
	}, 15*time.Second, 200*time.Millisecond)
	time.Sleep(time.Second)
	require.NoError(t, os.WriteFile(filepath.Join(gate, "release"), nil, 0666))
	finish(t, b, initial.Name, blocker)
	for _, q := range queued {
		require.Zero(t, finish(t, q.s, q.name, q.d).Result.ExitCode)
	}
	order, err := os.ReadFile(filepath.Join(gate, "order"))
	require.NoError(t, err)
	sort.Slice(priorities, func(i, j int) bool { return priorities[i] < priorities[j] })
	require.Equal(t, fmt.Sprintf("%d\n%d\n%d\n", priorities[0], priorities[1], priorities[2]), string(order), "seed=20261013")
}

func TestExecuteAfterCacheEviction(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{harness.App: {"cache": harness.Config{"max_size_bytes": int64(4 << 20), "pebble": harness.Config{"min_eviction_age": "0s", "atime_update_threshold": "0s", "sampler_iter_refresh_peroid": "100ms"}}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	rng := rand.New(rand.NewSource(20261013))
	priorities := []int32{-1000, 1000}
	digests := make([]*repb.Digest, 2)
	for i, p := range priorities {
		d := uploadAction(t, c, ctx, fmt.Sprintf("printf eviction-%d", p), false)
		digests[i] = d
		r := executeRequest(t, c, ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, ResultsCachePolicy: &repb.ResultsCachePolicy{Priority: p}})
		require.False(t, r.CachedResult)
		// Make AC entries large enough to compete with CAS entries for space.
		payload := make([]byte, 1<<20)
		_, err := rng.Read(payload)
		require.NoError(t, err)
		_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: "streams", ActionDigest: d, ActionResult: &repb.ActionResult{StdoutRaw: payload}, ResultsCachePolicy: &repb.ResultsCachePolicy{Priority: p}})
		require.NoError(t, err)
		hit := executeRequest(t, c, ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, ResultsCachePolicy: &repb.ResultsCachePolicy{Priority: p}})
		require.True(t, hit.CachedResult)
	}
	// Both priorities are hints. REAPI does not require a total eviction order;
	// the invariant is that an evicted result cannot produce a stale cache hit.
	for i := 0; i < 32; i++ {
		payload := make([]byte, 1<<20)
		_, err := rng.Read(payload)
		require.NoError(t, err)
		_, err = c.Upload(ctx, "eviction-pressure", payload)
		require.NoError(t, err)
	}
	gone := make([]bool, 2)
	require.Eventually(t, func() bool {
		for i, d := range digests {
			if gone[i] {
				continue
			}
			_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: "streams", ActionDigest: d})
			if status.Code(err) == codes.NotFound {
				gone[i] = true
				t.Logf("seed=20261013 observed eviction priority=%d", priorities[i])
			} else {
				require.NoError(t, err)
			}
		}
		return gone[0] && gone[1]
	}, 45*time.Second, time.Second)
	for i, p := range priorities {
		// Restore possibly evicted input CAS entries before Execute.
		d := uploadAction(t, c, ctx, fmt.Sprintf("printf eviction-%d", p), false)
		require.Equal(t, d.Hash, digests[i].Hash)
		r := executeRequest(t, c, ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d, ResultsCachePolicy: &repb.ResultsCachePolicy{Priority: p}})
		require.False(t, r.CachedResult)
		require.Zero(t, r.Result.ExitCode)
		require.True(t, executeRequest(t, c, ctx, &repb.ExecuteRequest{InstanceName: "streams", ActionDigest: d}).CachedResult)
	}
}
