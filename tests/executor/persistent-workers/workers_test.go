package workers_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

var seed = flag.Int64("workers.seed", 314, "replay seed")
var iterations = flag.Int("workers.iterations", 8, "cases per protocol (1..50)")

type observation struct {
	Arguments []string
	Content   string
	PID       int
	Count     int
}
type fixture struct {
	c      *harness.Clients
	binary []byte
}

func start(t *testing.T) *fixture {
	matches, err := filepath.Glob(filepath.Join(os.Getenv("TEST_SRCDIR"), "*", "tests/executor/persistent-workers/worker_/worker"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	b, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	return &fixture{f.Clients(harness.App), b}
}
func (f *fixture) run(t *testing.T, protocol, key, content string, args []string, persistent bool) (*repb.ExecuteResponse, observation) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	instance := "persistent-workers"
	files := []*repb.FileNode{}
	for _, in := range []struct {
		name       string
		data       []byte
		executable bool
	}{{"args.txt", []byte(strings.Join(args, "\n") + "\n"), false}, {"input.txt", []byte(content), false}, {"worker", f.binary, true}} {
		d, err := f.c.Upload(ctx, instance, in.data)
		require.NoError(t, err)
		files = append(files, &repb.FileNode{Name: in.name, Digest: d, IsExecutable: in.executable})
	}
	root, err := f.c.UploadProto(ctx, instance, &repb.Directory{Files: files})
	require.NoError(t, err)
	props := []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}
	if persistent {
		props = append(props, &repb.Platform_Property{Name: "persistentWorkerProtocol", Value: protocol})
		if key != "" {
			props = append(props, &repb.Platform_Property{Name: "persistentWorkerKey", Value: key})
		} else {
			props = append(props, &repb.Platform_Property{Name: "persistent-workers", Value: "true"}, &repb.Platform_Property{Name: "recycle-runner", Value: "true"})
		}
	}
	sort.Slice(props, func(i, j int) bool { return props[i].Name < props[j].Name })
	cmd, err := f.c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"./worker", protocol, "@args.txt"}, Platform: &repb.Platform{Properties: props}})
	require.NoError(t, err)
	action, err := f.c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(20 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	stream, err := f.c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	for {
		op, err := stream.Recv()
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		rsp := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(rsp))
		var o observation
		if rsp.GetStatus().GetCode() == 0 && rsp.GetResult().GetExitCode() == 0 {
			b := rsp.Result.StderrRaw
			if rsp.Result.StderrDigest != nil {
				b, err = f.c.Read(ctx, instance, rsp.Result.StderrDigest)
				require.NoError(t, err)
			}
			require.NoError(t, json.Unmarshal(b, &o), "stderr=%q", b)
		}
		return rsp, o
	}
}
func success(t *testing.T, r *repb.ExecuteResponse) {
	t.Helper()
	require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
	require.NotNil(t, r.Result)
	require.Zero(t, r.Result.ExitCode)
}
func TestProtocolReuseAndUpdatedInputs(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	require.True(t, *iterations >= 1 && *iterations <= 50)
	f := start(t)
	rng := rand.New(rand.NewSource(*seed))
	for _, protocol := range []string{"json", "proto"} {
		t.Run(protocol, func(t *testing.T) {
			var pid int
			for i := 0; i < *iterations; i++ {
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					t.Logf("seed=%d iteration=%d protocol=%s", *seed, i, protocol)
					alphabet := []rune("abcXYZ 012é中\\\"\t")
					size := rng.Intn(1024)
					if i == 0 {
						size = 0
					}
					if i == 1 {
						size = 1024
					}
					content := make([]rune, size)
					for j := range content {
						content[j] = alphabet[rng.Intn(len(alphabet))]
					}
					args := []string{"", fmt.Sprintf("case=%d", i), string(content[:len(content)/2])}
					r, baseline := f.run(t, protocol, "", string(content), args, false)
					success(t, r)
					require.Equal(t, string(content), baseline.Content)
					require.Equal(t, args, baseline.Arguments)
					r, worker := f.run(t, protocol, protocol+"-reuse", string(content), args, true)
					success(t, r)
					require.Equal(t, baseline.Content, worker.Content)
					require.Equal(t, baseline.Arguments, worker.Arguments)
					if i == 0 {
						pid = worker.PID
					}
					require.Equal(t, pid, worker.PID, "same key must reuse worker")
					require.Equal(t, i+1, worker.Count)
				})
			}
			r, separate := f.run(t, protocol, protocol+"-other", "separate", []string{"key"}, true)
			success(t, r)
			require.NotEqual(t, pid, separate.PID)
			require.Equal(t, 1, separate.Count)
		})
	}
}
func TestWorkerExitRecovery(t *testing.T) {
	t.Logf("seed=%d", *seed)
	f := start(t)
	for _, protocol := range []string{"json", "proto"} {
		t.Run(protocol, func(t *testing.T) {
			r, before := f.run(t, protocol, protocol+"-crash", "before", []string{"ok"}, true)
			success(t, r)
			r, _ = f.run(t, protocol, protocol+"-crash", "crash", []string{"CRASH"}, true)
			t.Logf("crash status=%v exit=%d", r.GetStatus(), r.GetResult().GetExitCode())
			require.True(t, r.GetStatus().GetCode() != 0 || r.GetResult().GetExitCode() != 0, "worker crash must fail request")
			r, after := f.run(t, protocol, protocol+"-crash", "after", []string{"recovered"}, true)
			success(t, r)
			require.Equal(t, "after", after.Content)
			require.NotEqual(t, before.PID, after.PID)
			require.Equal(t, 1, after.Count)
		})
	}
}
func TestConcurrentMultiplexCapableWorker(t *testing.T) {
	t.Logf("seed=%d", *seed)
	f := start(t)
	// The worker echoes IDs and accepts both singleplex and multiplex framing.
	// The released executor sends ID zero and may use multiple worker processes.
	for _, protocol := range []string{"json", "proto"} {
		t.Run(protocol, func(t *testing.T) {
			for i := 0; i < 4; i++ {
				i := i
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					t.Parallel()
					t.Logf("seed=%d concurrent=%d protocol=%s", *seed, i, protocol)
					content := fmt.Sprintf("seed=%d concurrent=%d", *seed, i)
					r, o := f.run(t, protocol, protocol+"-concurrent", content, []string{content}, true)
					success(t, r)
					require.Equal(t, content, o.Content)
					require.Equal(t, []string{content}, o.Arguments)
				})
			}
		})
	}
}

func TestPlatformProtocolAndDerivedKeys(t *testing.T) {
	t.Logf("seed=%d", *seed)
	f := start(t)
	for _, key := range []string{"same-explicit-key", ""} {
		t.Run(fmt.Sprintf("key=%q", key), func(t *testing.T) {
			pids := map[string]int{}
			for i := 0; i < 3; i++ {
				for _, protocol := range []string{"json", "proto"} {
					content := fmt.Sprintf("seed=%d protocol=%s iteration=%d", *seed, protocol, i)
					r, o := f.run(t, protocol, key, content, []string{content}, true)
					success(t, r)
					require.Equal(t, content, o.Content)
					require.Equal(t, []string{content}, o.Arguments)
					require.Positive(t, o.PID)
					if i == 0 {
						pids[protocol] = o.PID
					}
					require.Equal(t, pids[protocol], o.PID)
					require.Equal(t, i+1, o.Count)
				}
			}
			require.NotEqual(t, pids["json"], pids["proto"], "different protocol properties must isolate workers even with identical explicit keys")
		})
	}
}
