package upgradecompat_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Replace just one component, preserving peers, Redis, ports, config and /data.
// The fixture still owns the stopped original container and its volume.
func replaceComponent(t *testing.T, f *harness.Fixture, component harness.Component, image string, edit func(string) string) {
	t.Helper()
	docker := fixtureDocker(t, f)
	ep := f.Endpoint(component)
	name := ep.Container + "-round2"
	config := filepath.Join(f.OutputDir, string(component)+"-round2.yaml")
	b, err := os.ReadFile(filepath.Join(f.OutputDir, string(component)+".yaml"))
	require.NoError(t, err)
	if edit != nil {
		b = []byte(edit(string(b)))
	}
	require.NoError(t, os.WriteFile(config, b, 0600))
	// Pull before interrupting the workload; image-download time is not downtime.
	docker("pull", image)
	docker("stop", "--time", "30", ep.Container)
	docker("network", "disconnect", f.Network, ep.Container)
	args := []string{"create", "--name", name, "--network", f.Network, "--hostname", ep.Container, "--network-alias", string(component), "--network-alias", ep.Container, "--volumes-from", ep.Container, "--mount", "type=bind,source=" + config + ",target=/config.yaml,readonly",
		"-p", strings.TrimPrefix(ep.HTTP, "http://") + ":8080", "-p", ep.GRPC + ":1985", image, "--config_file=/config.yaml"}
	docker(args...)
	t.Cleanup(func() {
		// Preserve replacement diagnostics alongside the harness-owned originals.
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, string(component)+"-round2.log"), []byte(docker("logs", name)), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, string(component)+"-round2.inspect.json"), []byte(docker("inspect", name)), 0600))
		docker("rm", "-f", name)
	})
	docker("start", name)
	client := &http.Client{Timeout: 2 * time.Second}
	ready := false
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		r, err := client.Get(ep.HTTP + "/readyz?server-type=" + ep.ServerType)
		if err != nil {
			continue
		}
		r.Body.Close()
		if r.StatusCode == 200 {
			ready = true
			break
		}
	}
	require.True(t, ready, "replacement not ready: %s", docker("logs", name))
}

func schedulerRecovery(t *testing.T, checkRunning bool, from, to string) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor},
		Images:     map[harness.Component]string{harness.App: from, harness.Executor: harness.PinnedImages[harness.Executor]},
		Config:     map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"custom_resources": []harness.Config{{"name": "upgrade-slot", "value": 1}}}}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	docker := fixtureDocker(t, f)
	executor := f.Endpoint(harness.Executor).Container
	root, err := c.UploadProto(ctx, "scheduler-upgrade", &repb.Directory{})
	require.NoError(t, err)
	scripts := []string{
		"touch /tmp/upgrade-started; while [ ! -f /tmp/upgrade-release ]; do sleep 1; done; printf 'running survived\\n'",
		"printf 'queued survived\\n'",
	}
	if checkRunning {
		scripts = scripts[:1]
	}
	names := make([]string, len(scripts))
	for i, script := range scripts {
		command, err := c.UploadProto(ctx, "scheduler-upgrade", &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}, {Name: "resources:upgrade-slot", Value: "1"}}}})
		require.NoError(t, err)
		action, err := c.UploadProto(ctx, "scheduler-upgrade", &repb.Action{CommandDigest: command, InputRootDigest: root, Timeout: durationpb.New(2 * time.Minute)})
		require.NoError(t, err)
		stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "scheduler-upgrade", ActionDigest: action, SkipCacheLookup: true})
		require.NoError(t, err)
		for {
			op, err := stream.Recv()
			require.NoError(t, err)
			require.False(t, op.Done)
			names[i] = op.Name
			metadata := &repb.ExecuteOperationMetadata{}
			require.NoError(t, op.Metadata.UnmarshalTo(metadata))
			if i == 0 && metadata.Stage == repb.ExecutionStage_EXECUTING {
				break
			}
			if i == 1 && metadata.Stage == repb.ExecutionStage_QUEUED {
				break
			}
		}
		if i == 0 {
			require.Equal(t, "started", docker("exec", executor, "/bin/sh", "-c", "while [ ! -f /tmp/upgrade-started ]; do sleep 1; done; echo started"))
		}
	}
	// One custom resource slot is held by the gated running action. The other
	// task is acknowledged by the live executor but cannot acquire that slot.
	if !checkRunning {
		redis := f.ID + "-redis"
		key := "task/" + names[1]
		if docker("exec", redis, "redis-cli", "--raw", "EXISTS", key) == "0" {
			key = "task/{" + names[1] + "}"
		}
		require.Equal(t, "1", docker("exec", redis, "redis-cli", "--raw", "HEXISTS", key, "taskProto"))
		require.Equal(t, "0", docker("exec", redis, "redis-cli", "--raw", "HEXISTS", key, "claimed"))
	}
	t.Logf("replacing app %s -> %s; operations=%v", from, to, names)
	replaceComponent(t, f, harness.App, to, nil)
	client := &http.Client{Timeout: 2 * time.Second}
	require.Eventually(t, func() bool {
		r, err := client.Get(f.Endpoint(harness.Executor).HTTP + "/readyz?server-type=" + f.Endpoint(harness.Executor).ServerType)
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	}, 60*time.Second, time.Second, "executor must reconnect before checking retained operations")
	c = f.Clients(harness.App)
	docker("exec", executor, "touch", "/tmp/upgrade-release")
	for i, name := range names {
		if !checkRunning && i == 0 {
			continue
		}
		// Resume the original operations; never resubmit Execute.
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
			require.Equal(t, []string{"running survived\n", "queued survived\n"}[i], string(output))
			break
		}
	}
}

// Previous non-prerelease GitHub release before v2.314.0; linux/amd64
// digests resolved using the GCR manifest API, never mutable tags.
const oldApp = "gcr.io/flame-public/buildbuddy-app-enterprise@sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6"
const oldExecutor = "gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:8f7c2662f5fcd0ab888113f6d36d31cde8b8ea254434a8177cf6996b01155afe"

// Discover only the daemon owning this fixture; the harness may start a private
// daemon rather than using the runner's default socket.
func fixtureDocker(t *testing.T, f *harness.Fixture) func(...string) string {
	t.Helper()
	sockets, err := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	require.NoError(t, err)
	hosts := append([]string{""}, sockets...)
	for _, host := range hosts {
		prefix := []string{}
		if host != "" {
			prefix = []string{"--host", "unix://" + host}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := exec.CommandContext(ctx, "docker", append(prefix, "inspect", f.Endpoint(harness.App).Container)...).CombinedOutput()
		cancel()
		if err != nil {
			continue
		}
		return func(args ...string) string {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			b, err := exec.CommandContext(ctx, "docker", append(prefix, args...)...).CombinedOutput()
			require.NoError(t, err, "docker %v: %s", args, b)
			return strings.TrimSpace(string(b))
		}
	}
	t.Fatal("cannot locate fixture's Docker daemon")
	return nil
}
