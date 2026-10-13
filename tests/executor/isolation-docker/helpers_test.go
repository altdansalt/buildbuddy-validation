package isolation_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
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
	"google.golang.org/protobuf/types/known/durationpb"
)

// Own the daemon as well as its containers: socket access never reaches another
// worker's containers. Register daemon cleanup before the fixture so it runs last.
func fixture(t *testing.T) *harness.Fixture {
	return providerFixture(t, "docker")
}

func providerFixture(t *testing.T, provider string) *harness.Fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "bbval-isolation-")
	require.NoError(t, err)
	socket := filepath.Join(dir, "docker.sock")
	log, err := os.Create(filepath.Join(dir, "dockerd.log"))
	require.NoError(t, err)
	var pool [2]byte
	_, err = rand.Read(pool[:])
	require.NoError(t, err)
	poolCIDR := fmt.Sprintf("10.%d.%d.0/24", pool[0], pool[1])
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sudo", "-n", "dockerd", "--host=unix://"+socket,
		"--data-root="+filepath.Join(dir, "docker"), "--exec-root="+filepath.Join(dir, "exec"),
		"--pidfile="+filepath.Join(dir, "pid"), "--registry-mirror=https://mirror.gcr.io", "--insecure-registry=10.0.0.0/8", "--bridge=none", "--iptables=false", "--ip-masq=false", "--default-address-pool=base="+poolCIDR+",size=28")
	cmd.Stdout = log
	cmd.Stderr = log
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		// Signal dockerd itself; killing sudo alone leaves root-owned processes alive.
		if pid, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
			exec.Command("sudo", "-n", "kill", "-TERM", strings.TrimSpace(string(pid))).Run()
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			cancel()
			t.Error("dockerd shutdown timed out")
		}
		cancel()
		log.Close()
		if out := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); out != "" {
			b, _ := os.ReadFile(filepath.Join(dir, "dockerd.log"))
			require.NoError(t, os.WriteFile(filepath.Join(out, "isolation-dockerd.log"), b, 0600))
		}
		b, err := exec.Command("sudo", "-n", "rm", "-rf", "--", dir).CombinedOutput()
		require.NoError(t, err, string(b))
	})
	t.Setenv("DOCKER_HOST", "unix://"+socket)
	deadline := time.Now().Add(time.Minute)
	for {
		if exec.Command("docker", "info").Run() == nil {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(log.Name())
			t.Fatalf("private dockerd not ready: %s", b)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Reused hosted runners may retain a DROP forwarding policy from another
	// Docker daemon. Permit only traffic within this private daemon's random
	// address pool; never change the runner's global forwarding policy.
	rule := []string{"-w", "-s", poolCIDR, "-d", poolCIDR, "-m", "comment", "--comment", filepath.Base(dir), "-j", "ACCEPT"}
	insert := append([]string{"-n", "iptables", "-I", "FORWARD", "1"}, rule...)
	out, err := exec.Command("sudo", insert...).CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() {
		remove := append([]string{"-n", "iptables", "-D", "FORWARD"}, rule...)
		out, err := exec.Command("sudo", remove...).CombinedOutput()
		require.NoError(t, err, string(out))
	})
	b, err := exec.Command("docker", "network", "create", "action-network").CombinedOutput()
	require.NoError(t, err, string(b))
	t.Cleanup(func() {
		b, err := exec.Command("docker", "network", "rm", "action-network").CombinedOutput()
		require.NoError(t, err, string(b))
	})
	root := filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(root, 0777))
	extra := filepath.Join(dir, "extra")
	require.NoError(t, os.MkdirAll(extra, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(extra, "marker"), []byte("read-only mount\n"), 0644))
	opts := harness.Options{
		Components: []harness.Component{harness.App, harness.Executor},
		Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{
			"docker_socket": "/var/run/docker.sock", "root_directory": filepath.Join(root, "remotebuilds"),
			"host_root_directory": root, "local_cache_directory": filepath.Join(root, "filecache"),
			"default_isolation_type": "docker", "default_image": "mirror.gcr.io/library/alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c",
			"docker_network": "action-network", "docker_volumes": []string{extra + ":/conformance-extra:ro"},
		}}},
		DockerArgs: map[harness.Component][]string{harness.Executor: {"--volume", socket + ":/var/run/docker.sock", "--volume", root + ":" + root, "--env", "DOCKER_HOST=unix:///var/run/docker.sock"}},
	}
	if provider != "docker" {
		opts.Args = map[harness.Component][]string{harness.Executor: {"--executor.enable_" + provider + "=true", "--executor.container_registry_allowed_private_ips=127.0.0.0/8"}}
		opts.DockerArgs[harness.Executor] = append(opts.DockerArgs[harness.Executor], "--privileged", "--cgroupns=host", "--volume", "/sys/fs/cgroup:/sys/fs/cgroup:rw")
		if provider == "podman" {
			// Podman resolves action bind sources inside the executor container,
			// unlike Docker's host daemon. Expose the same source at the same path.
			opts.DockerArgs[harness.Executor] = append(opts.DockerArgs[harness.Executor], "--volume", extra+":"+extra+":ro")
			opts.Args[harness.Executor] = append(opts.Args[harness.Executor], "--executor.podman.storage_driver=vfs", "--executor.podman.enable_stats=false")
		} else {
			opts.Args[harness.Executor] = append(opts.Args[harness.Executor], "--executor.oci.default_network_mode=off")
		}
	}
	return harness.Start(t, opts)
}

func execute(t *testing.T, f *harness.Fixture, image, script, wd string, props map[string]string) *repb.ExecuteResponse {
	t.Helper()
	return executeTimeout(t, f, image, script, wd, props, 40*time.Second)
}

func executeTimeout(t *testing.T, f *harness.Fixture, image, script, wd string, props map[string]string, timeout time.Duration) *repb.ExecuteResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	instance := "docker-isolation"
	input, err := c.Upload(ctx, instance, []byte("mounted input\n"))
	require.NoError(t, err)
	sub, err := c.UploadProto(ctx, instance, &repb.Directory{Files: []*repb.FileNode{{Name: "input.txt", Digest: input}}})
	require.NoError(t, err)
	root, err := c.UploadProto(ctx, instance, &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "sub", Digest: sub}}})
	require.NoError(t, err)
	p := map[string]string{"OSFamily": "linux", "Arch": "amd64", "workload-isolation-type": "docker", "container-image": "docker://" + image}
	for k, v := range props {
		p[k] = v
	}
	platform := &repb.Platform{}
	// REAPI requires properties to be lexicographically sorted.
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		platform.Properties = append(platform.Properties, &repb.Platform_Property{Name: k, Value: p[k]})
	}
	command, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, WorkingDirectory: wd, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "CONFORMANCE_VALUE", Value: "spaces = and unicode λ"}}, OutputPaths: []string{"output.txt"}, Platform: platform})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: command, InputRootDigest: root, Timeout: durationpb.New(timeout), DoNotCache: true})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	for {
		op, err := stream.Recv()
		if err == io.EOF {
			t.Fatal("stream ended without response")
		}
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		r := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(r))
		t.Logf("status=%v exit=%d", r.Status, r.GetResult().GetExitCode())
		return r
	}
}
func stdout(t *testing.T, f *harness.Fixture, r *repb.ExecuteResponse) string {
	t.Helper()
	if r.GetResult().GetStdoutDigest() == nil {
		return string(r.GetResult().GetStdoutRaw())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := f.Clients(harness.App).Read(ctx, "docker-isolation", r.Result.StdoutDigest)
	require.NoError(t, err)
	return string(b)
}
func stderr(t *testing.T, f *harness.Fixture, r *repb.ExecuteResponse) string {
	t.Helper()
	if r.GetResult().GetStderrDigest() == nil {
		return string(r.GetResult().GetStderrRaw())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := f.Clients(harness.App).Read(ctx, "docker-isolation", r.Result.StderrDigest)
	require.NoError(t, err)
	return string(b)
}
func success(t *testing.T, r *repb.ExecuteResponse) {
	t.Helper()
	require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
	require.NotNil(t, r.Result)
	require.Zero(t, r.Result.ExitCode)
}
