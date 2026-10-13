package upgradecompat_test

import (
	"context"
	"math/rand"
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

const oldProxy = "gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:111df7a8d4bb57993bc78f8ed18458f32a43a93e5028f906f4c6e537e10434c6"

// Replace just one component, preserving peers, Redis, ports, config and /data.
// The fixture still owns the stopped original container and its volume.
func replaceComponent(t *testing.T, f *harness.Fixture, component harness.Component, image string, edit func(string) string) {
	t.Helper()
	docker := fixtureDocker(t, f)
	ep := f.Endpoint(component)
	name := ep.Container + "-round2"
	config := filepath.Join(t.TempDir(), "config.yaml")
	docker("cp", ep.Container+":/config.yaml", config)
	if edit != nil {
		b, err := os.ReadFile(config)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(config, []byte(edit(string(b))), 0600))
	}
	// Pull before interrupting the workload; image-download time is not downtime.
	docker("pull", image)
	docker("stop", "--time", "30", ep.Container)
	docker("network", "disconnect", f.Network, ep.Container)
	args := []string{"create", "--name", name, "--network", f.Network, "--network-alias", string(component), "--volumes-from", ep.Container,
		"-p", strings.TrimPrefix(ep.HTTP, "http://") + ":8080", "-p", ep.GRPC + ":1985", image, "--config_file=/config.yaml"}
	docker(args...)
	t.Cleanup(func() {
		// Preserve replacement diagnostics alongside the harness-owned originals.
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, string(component)+"-round2.log"), []byte(docker("logs", name)), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, string(component)+"-round2.inspect.json"), []byte(docker("inspect", name)), 0600))
		docker("rm", "-f", name)
	})
	docker("cp", config, name+":/config.yaml")
	docker("start", name)
	client := &http.Client{Timeout: 2 * time.Second}
	require.Eventually(t, func() bool {
		r, err := client.Get(ep.HTTP + "/readyz?server-type=" + ep.ServerType)
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	}, 90*time.Second, time.Second)
}

func TestProxyPersistenceUpgrade(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Images: map[harness.Component]string{harness.App: harness.PinnedImages[harness.App], harness.CacheProxy: oldProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	const seed int64 = 2026101202
	t.Logf("payload seed=%d; proxy %s -> %s", seed, oldProxy, harness.PinnedImages[harness.CacheProxy])
	rng := rand.New(rand.NewSource(seed))
	payloads := make([][]byte, 12)
	digests := make([]*repb.Digest, len(payloads))
	for i := range payloads {
		payloads[i] = make([]byte, 1+rng.Intn(256*1024))
		_, err := rng.Read(payloads[i])
		require.NoError(t, err)
		digests[i], err = app.Upload(ctx, "proxy-upgrade", payloads[i])
		require.NoError(t, err)
		b, err := proxy.Read(ctx, "proxy-upgrade", digests[i])
		require.NoError(t, err)
		require.Equal(t, payloads[i], b)
	}
	// Flush Pebble gracefully, then deny cache fallback while retaining the local
	// app as authenticator. Successful reads must come from the retained volume.
	replaceComponent(t, f, harness.CacheProxy, harness.PinnedImages[harness.CacheProxy], func(config string) string {
		require.Contains(t, config, "remote_cache: grpc://app:1985")
		return strings.Replace(config, "remote_cache: grpc://app:1985", "remote_cache: grpc://127.0.0.1:1", 1)
	})
	proxy = f.Clients(harness.CacheProxy)
	for _, i := range rng.Perm(len(payloads)) {
		b, err := proxy.Read(ctx, "proxy-upgrade", digests[i])
		require.NoError(t, err, "seed=%d blob=%d", seed, i)
		require.Equal(t, payloads[i], b)
	}
	// A never-warmed remote blob must fail, establishing that fallback is blocked.
	d, err := app.Upload(ctx, "proxy-upgrade", []byte("remote-only control"))
	require.NoError(t, err)
	short, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	_, err = proxy.Read(short, "proxy-upgrade", d)
	require.Error(t, err)
}

func TestSchedulerUpgrade(t *testing.T) {
	for _, phase := range []string{"running", "queued"} {
		t.Run(phase, func(t *testing.T) {
			f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Images: map[harness.Component]string{harness.App: oldApp, harness.Executor: harness.PinnedImages[harness.Executor]}})
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()
			c := f.Clients(harness.App)
			docker := fixtureDocker(t, f)
			executor := f.Endpoint(harness.Executor).Container
			if phase == "queued" {
				docker("stop", "--time", "30", executor)
			}
			root, err := c.UploadProto(ctx, "scheduler-upgrade", &repb.Directory{})
			require.NoError(t, err)
			script := "printf 'queue survived\\n'"
			if phase == "running" {
				script = "touch /tmp/upgrade-started; while [ ! -f /tmp/upgrade-release ]; do sleep 1; done; printf 'queue survived\\n'"
			}
			command, err := c.UploadProto(ctx, "scheduler-upgrade", &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
			require.NoError(t, err)
			action, err := c.UploadProto(ctx, "scheduler-upgrade", &repb.Action{CommandDigest: command, InputRootDigest: root, Timeout: durationpb.New(4 * time.Minute)})
			require.NoError(t, err)
			stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "scheduler-upgrade", ActionDigest: action, SkipCacheLookup: true})
			require.NoError(t, err)
			var name string
			for {
				op, err := stream.Recv()
				require.NoError(t, err)
				require.False(t, op.Done)
				name = op.Name
				metadata := &repb.ExecuteOperationMetadata{}
				require.NoError(t, op.Metadata.UnmarshalTo(metadata))
				if phase == "queued" && metadata.Stage == repb.ExecutionStage_QUEUED {
					break
				}
				if phase == "running" && metadata.Stage == repb.ExecutionStage_EXECUTING {
					break
				}
			}
			if phase == "running" {
				require.Equal(t, "started", docker("exec", executor, "/bin/sh", "-c", "while [ ! -f /tmp/upgrade-started ]; do sleep 1; done; echo started"))
			}
			t.Logf("upgrading app with %s operation %s", phase, name)
			replaceComponent(t, f, harness.App, harness.PinnedImages[harness.App], nil)
			if phase == "queued" {
				docker("start", executor)
			} else {
				docker("exec", executor, "touch", "/tmp/upgrade-release")
			}
			// Streams cannot survive process replacement. Resume by the original name;
			// never resubmit Execute, which would hide loss of the original queue entry.
			resumed, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
			require.NoError(t, err)
			for {
				op, err := resumed.Recv()
				require.NoError(t, err)
				if !op.Done {
					continue
				}
				require.Nil(t, op.GetError())
				response := &repb.ExecuteResponse{}
				require.NoError(t, op.GetResponse().UnmarshalTo(response))
				require.Zero(t, response.GetStatus().GetCode(), "%v", response.GetStatus())
				require.NotNil(t, response.Result)
				require.Zero(t, response.Result.ExitCode)
				output := response.Result.StdoutRaw
				if response.Result.StdoutDigest != nil {
					output, err = c.Read(ctx, "scheduler-upgrade", response.Result.StdoutDigest)
					require.NoError(t, err)
				}
				require.Equal(t, "queue survived\n", string(output))
				break
			}
		})
	}
}
