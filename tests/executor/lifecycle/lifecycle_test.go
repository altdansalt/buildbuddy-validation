package lifecycle_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
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

func probe(ep *harness.Endpoint, path string, header bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	url := ep.HTTP + path
	if !header {
		url += "?server-type=" + ep.ServerType
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	if header {
		req.Header.Set("server-type", ep.ServerType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// REAPI Execute must deliver a completed operation containing ExecuteResponse;
// observing EXECUTING alone is not proof that the command has started.
func execute(t *testing.T, f *harness.Fixture, script string, started func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	instance := "lifecycle"
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{
		Arguments: []string{"/bin/sh", "-c", script},
		Platform: &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"},
		}},
	})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(60 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	triggered := false
	for {
		op, err := stream.Recv()
		require.NoError(t, err, "Execute ended before completion")
		if !op.Done {
			md := &repb.ExecuteOperationMetadata{}
			require.NoError(t, op.GetMetadata().UnmarshalTo(md))
			if md.Stage == repb.ExecutionStage_EXECUTING && started != nil && !triggered {
				triggered = true
				started()
			}
			continue
		}
		require.Nil(t, op.GetError())
		resp := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(resp))
		require.Zero(t, resp.GetStatus().GetCode(), "%v", resp.GetStatus())
		require.NotNil(t, resp.Result)
		require.Zero(t, resp.Result.ExitCode)
		b := resp.Result.StdoutRaw
		if resp.Result.StdoutDigest != nil {
			b, err = c.Read(ctx, instance, resp.Result.StdoutDigest)
			require.NoError(t, err)
		}
		require.Equal(t, "lifecycle-ok\n", string(b))
		if started != nil {
			require.True(t, triggered, "no EXECUTING update")
		}
		return
	}
}

func TestRegistrationHealthAndRestart(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	ep := f.Endpoint(harness.Executor)
	for _, path := range []string{"/healthz", "/readyz"} {
		require.Equal(t, 200, probe(ep, path, false))
		require.Equal(t, 200, probe(ep, path, true))
	}
	execute(t, f, "printf 'lifecycle-ok\\n'", nil)
	f.Stop(harness.Executor)
	require.Zero(t, f.WaitExit(harness.Executor, false))
	f.Start(harness.Executor)
	require.Eventually(t, func() bool { return probe(ep, "/readyz", false) == 200 }, time.Minute, 250*time.Millisecond)
	execute(t, f, "printf 'lifecycle-ok\\n'", nil)
	logs := f.Logs(harness.Executor)
	require.NotContains(t, logs, "panic:")
	require.NotContains(t, logs, "fatal error:")
}

func TestGracefulDrainInFlight(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Args: map[harness.Component][]string{
		harness.Executor: {"--max_shutdown_duration=40s", "--shutdown_lameduck_duration=2s"},
	}})
	ep := f.Endpoint(harness.Executor)
	marker := "/tmp/" + f.ID + "-started"
	execute(t, f, fmt.Sprintf("touch %s; sleep 8; printf 'lifecycle-ok\\n'", marker), func() {
		require.Eventually(t, func() bool {
			return f.Exec(harness.Executor, "sh", "-c", "if test -f "+marker+"; then echo started; fi") == "started"
		}, 15*time.Second, 100*time.Millisecond)
		f.Kill(harness.Executor, "TERM")
		require.Eventually(t, func() bool { return probe(ep, "/readyz", false) == 503 }, time.Second, 50*time.Millisecond)
		require.Equal(t, 200, probe(ep, "/healthz", false))
	})
	require.Zero(t, f.WaitExit(harness.Executor, false))
	logs := f.Logs(harness.Executor)
	require.NotContains(t, logs, "Non-graceful exit")
	require.NotContains(t, logs, "panic:")
}

func TestAdjacentReleaseSkew(t *testing.T) {
	for _, older := range []harness.Component{harness.Executor, harness.App} {
		t.Run(string(older)+"-v2.313.0", func(t *testing.T) {
			f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Images: map[harness.Component]string{older: "enterprise-v2.313.0"}})
			execute(t, f, "printf 'lifecycle-ok\\n'", nil)
		})
	}
}

func TestInvalidBuildRootFailsAndRecovers(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor},
		Config:               map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"root_directory": "/dev/null/executor"}}},
		ExpectStartupFailure: map[harness.Component]bool{harness.Executor: true},
	})
	require.NotZero(t, f.WaitExit(harness.Executor, true))
	require.Contains(t, f.Logs(harness.Executor), "not a directory")
	require.NotContains(t, f.Logs(harness.Executor), "panic:")
	original := filepath.Join(f.OutputDir, "executor.yaml")
	data, err := os.ReadFile(original)
	require.NoError(t, err)
	recovered := filepath.Join(f.OutputDir, "executor-recovered.yaml")
	require.NoError(t, os.WriteFile(recovered, []byte(strings.ReplaceAll(string(data), "/dev/null/executor", "/data/executor")), 0600))
	f.CopyConfig(harness.Executor, recovered)
	f.Restart(harness.Executor)
	execute(t, f, "printf 'lifecycle-ok\\n'", nil)
}

func TestAdditionalExecutorAndRestart(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	extra := f.AddExecutor(harness.Options{})
	f.Stop(harness.Executor)
	f.Restart(extra)
	execute(t, f, "printf 'lifecycle-ok\\n'", nil)
}
