package workers_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
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
		props = append(props, &repb.Platform_Property{Name: "persistentWorkerKey", Value: key}, &repb.Platform_Property{Name: "persistentWorkerProtocol", Value: protocol})
	}
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
					content := make([]rune, rng.Intn(1024))
					for j := range content {
						content[j] = alphabet[rng.Intn(len(alphabet))]
					}
					args := []string{"", fmt.Sprintf("case=%d", i), string(content[:len(content)/2])}
					r, baseline := f.run(t, protocol, "", string(content), args, false)
					success(t, r)
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
