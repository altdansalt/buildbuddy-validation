package platformprops_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

// Own the daemon as well as its containers: socket access never reaches another
// worker's containers. Register daemon cleanup before the fixture so it runs last.
func dockerFixture(t *testing.T) *harness.Fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "bbval-platform-props-")
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
		"--pidfile="+filepath.Join(dir, "pid"), "--bridge=none", "--default-address-pool=base="+poolCIDR+",size=28")
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
			require.NoError(t, os.WriteFile(filepath.Join(out, "platform-props-dockerd.log"), b, 0600))
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
	return harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor},
		Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{
			"docker_socket": "/var/run/docker.sock", "root_directory": filepath.Join(root, "remotebuilds"),
			"host_root_directory": root, "local_cache_directory": filepath.Join(root, "filecache"),
			"default_isolation_type": "docker", "default_image": "alpine:3.21.3",
			"docker_network": "action-network",
		}}},
		DockerArgs: map[harness.Component][]string{harness.Executor: {"--volume", socket + ":/var/run/docker.sock", "--volume", root + ":" + root, "--env", "DOCKER_HOST=unix:///var/run/docker.sock"}},
	})
}
