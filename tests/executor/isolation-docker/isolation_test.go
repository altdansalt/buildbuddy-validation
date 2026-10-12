package isolation_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
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
	t.Helper()
	dir, err := os.MkdirTemp("", "bbval-isolation-")
	require.NoError(t, err)
	socket := filepath.Join(dir, "docker.sock")
	log, err := os.Create(filepath.Join(dir, "dockerd.log"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sudo", "-n", "dockerd", "--host=unix://"+socket,
		"--data-root="+filepath.Join(dir, "docker"), "--exec-root="+filepath.Join(dir, "exec"),
		"--pidfile="+filepath.Join(dir, "pid"), "--bridge=none", "--iptables=false", "--ip-masq=false")
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
	return harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor},
		Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{
			"docker_socket": "/var/run/docker.sock", "root_directory": filepath.Join(root, "remotebuilds"),
			"host_root_directory": root, "local_cache_directory": filepath.Join(root, "filecache"),
			"default_isolation_type": "docker", "default_image": "alpine:3.21.3",
			"docker_network": "action-network", "docker_volumes": []string{extra + ":/conformance-extra:ro"},
		}}},
		DockerArgs: map[harness.Component][]string{harness.Executor: {"--volume", socket + ":/var/run/docker.sock", "--volume", root + ":" + root, "--env", "DOCKER_HOST=unix:///var/run/docker.sock"}},
	})
}

func execute(t *testing.T, f *harness.Fixture, image, script, wd string, props map[string]string) *repb.ExecuteResponse {
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
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: command, InputRootDigest: root, Timeout: durationpb.New(40 * time.Second), DoNotCache: true})
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
		t.Logf("status=%v result=%v", r.Status, r.Result)
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
func success(t *testing.T, r *repb.ExecuteResponse) {
	t.Helper()
	require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
	require.NotNil(t, r.Result)
	require.Zero(t, r.Result.ExitCode)
}

func TestDockerIsolation(t *testing.T) {
	f := fixture(t)
	t.Run("tag_workdir_env_mount", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `cat input.txt > output.txt; printf '%s\n' "$CONFORMANCE_VALUE"; test -f /etc/alpine-release; test ! -S /var/run/docker.sock`, "sub", nil)
		success(t, r)
		require.Equal(t, "spaces = and unicode λ\n", stdout(t, f, r))
		require.Len(t, r.Result.OutputFiles, 1)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		b, err := f.Clients(harness.App).Read(ctx, "docker-isolation", r.Result.OutputFiles[0].Digest)
		require.NoError(t, err)
		require.Equal(t, "mounted input\n", string(b))
	})
	t.Run("network_on", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `test -d /sys/class/net/eth0; cat /proc/net/route`, "", nil)
		success(t, r)
		require.Contains(t, stdout(t, f, r), "eth0")
	})
	t.Run("readonly_mount", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `cat /conformance-extra/marker; if echo overwrite > /conformance-extra/marker 2>/dev/null; then exit 1; fi`, "", nil)
		success(t, r)
		require.Equal(t, "read-only mount\n", stdout(t, f, r))
	})
	t.Run("network_precedence", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `ls /sys/class/net`, "", map[string]string{"network": "off", "dockerNetwork": "host"})
		success(t, r)
		require.Equal(t, "lo\n", stdout(t, f, r))
	})
	t.Run("numeric_user", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", "id -u; id -g; cat sub/input.txt", "", map[string]string{"dockerUser": "12345:23456"})
		success(t, r)
		require.Equal(t, "12345\n23456\nmounted input\n", stdout(t, f, r))
	})
	for _, property := range []string{"network", "dockerNetwork"} {
		t.Run(property+"_off", func(t *testing.T) {
			r := execute(t, f, "alpine:3.21.3", `ls /sys/class/net; cat /proc/net/route`, "", map[string]string{property: "off"})
			success(t, r)
			out := stdout(t, f, r)
			require.True(t, strings.HasPrefix(out, "lo\n"), out)
			require.NotContains(t, out, "eth0")
		})
	}
	t.Run("fresh_container", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `echo dirty > /tmp/action-marker; echo dirty > sub/unlisted; echo first`, "", nil)
		success(t, r)
		r = execute(t, f, "alpine:3.21.3", `test ! -e /tmp/action-marker && test ! -e sub/unlisted && echo clean`, "", nil)
		success(t, r)
		require.Equal(t, "clean\n", stdout(t, f, r))
	})
	t.Run("nonzero_exit", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `echo expected >&2; exit 37`, "", nil)
		require.Zero(t, r.GetStatus().GetCode())
		require.EqualValues(t, 37, r.GetResult().GetExitCode())
	})
	t.Run("digest", func(t *testing.T) {
		b, err := exec.Command("docker", "image", "inspect", "alpine:3.21.3", "--format", "{{index .RepoDigests 0}}").CombinedOutput()
		require.NoError(t, err, string(b))
		ref := strings.TrimSpace(string(b))
		require.Contains(t, ref, "@sha256:")
		r := execute(t, f, ref, "cat /etc/alpine-release", "", nil)
		success(t, r)
		require.Equal(t, "3.21.3\n", stdout(t, f, r))
	})
	t.Run("invalid_network", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", "echo must-not-run", "", map[string]string{"network": "invalid"})
		require.EqualValues(t, 3, r.GetStatus().GetCode())
		require.Empty(t, stdout(t, f, r))
	})
	t.Run("private_registry_denied", func(t *testing.T) {
		var requests atomic.Int64
		registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			requests.Add(1)
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture-private-registry"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)
		}))
		defer registry.Close()
		// Docker treats loopback registries as insecure, so no shared daemon config
		// or external authentication provider is needed for this denial oracle.
		image := strings.TrimPrefix(registry.URL, "http://") + "/private:latest"
		r := execute(t, f, image, "echo must-not-run", "", nil)
		require.Positive(t, requests.Load(), "pull must reach our local registry")
		require.NotZero(t, r.GetStatus().GetCode())
		require.Empty(t, stdout(t, f, r))
		require.Contains(t, strings.ToLower(r.GetStatus().GetMessage()), "auth")
	})
	t.Run("missing_tag", func(t *testing.T) {
		r := execute(t, f, "alpine:bbval-nonexistent-tag-20261012", "echo must-not-run", "", nil)
		require.NotZero(t, r.GetStatus().GetCode())
		require.Empty(t, stdout(t, f, r))
		require.Contains(t, strings.ToLower(r.GetStatus().GetMessage()), "pull")
	})
	// No action containers should remain after completed non-recycled actions.
	deadline := time.Now().Add(15 * time.Second)
	for {
		b, err := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}").CombinedOutput()
		require.NoError(t, err)
		names := strings.Fields(string(b))
		if len(names) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("action containers remain: %s", b)
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, err := exec.Command("docker", "logs", f.Endpoint(harness.Executor).Container).CombinedOutput()
	require.NoError(t, err)
	require.NotContains(t, string(logs), "panic:")
	require.NotContains(t, string(logs), "fatal error:")
}
