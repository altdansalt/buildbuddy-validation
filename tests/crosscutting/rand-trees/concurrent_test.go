package randtrees_test

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

// Own the daemon as well as its containers: socket access never reaches another
// worker's containers. Register daemon cleanup before the fixture so it runs last.
func dockerFixture(t *testing.T) *harness.Fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "bbval-rand-trees-")
	require.NoError(t, err)
	socket := filepath.Join(dir, "docker.sock")
	log, err := os.Create(filepath.Join(dir, "dockerd.log"))
	require.NoError(t, err)
	var pool [2]byte
	_, err = cryptorand.Read(pool[:])
	require.NoError(t, err)
	poolCIDR := fmt.Sprintf("10.%d.%d.0/24", pool[0], pool[1])
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sudo", "-n", "dockerd", "--host=unix://"+socket,
		"--data-root="+filepath.Join(dir, "docker"), "--exec-root="+filepath.Join(dir, "exec"),
		"--pidfile="+filepath.Join(dir, "pid"), "--bridge=none", "--default-address-pool=base="+poolCIDR+",size=28")
	cmd.Stdout = log
	cmd.Stderr = log
	// Match the harness: serialize iptables initialization across owned daemons.
	lock, err := os.OpenFile(filepath.Join(os.TempDir(), "bbval-dockerd-start.lock"), os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	defer lock.Close()
	require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX))
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
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
			require.NoError(t, os.WriteFile(filepath.Join(out, "rand-trees-dockerd.log"), b, 0600))
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
	require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN))
	b, err := exec.Command("docker", "network", "create", "action-network").CombinedOutput()
	require.NoError(t, err, string(b))
	t.Cleanup(func() {
		b, err := exec.Command("docker", "network", "rm", "action-network").CombinedOutput()
		require.NoError(t, err, string(b))
	})
	root := filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(root, 0777))
	// The released executor image has a GCR credential helper configured. Public
	// image pulls need no credentials; use an empty Docker config for its CLI.
	dockerConfig := filepath.Join(dir, "docker-config")
	require.NoError(t, os.MkdirAll(dockerConfig, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte("{}"), 0644))
	return harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor},
		Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{
			"docker_socket": "/var/run/docker.sock", "root_directory": filepath.Join(root, "remotebuilds"),
			"host_root_directory": root, "local_cache_directory": filepath.Join(root, "filecache"),
			"default_isolation_type": "docker",
			"docker_network":         "action-network",
		}}},
		DockerArgs: map[harness.Component][]string{harness.Executor: {"--volume", socket + ":/var/run/docker.sock", "--volume", root + ":" + root, "--env", "DOCKER_HOST=unix:///var/run/docker.sock", "--volume", dockerConfig + ":/conformance-docker-config:ro", "--env", "DOCKER_CONFIG=/conformance-docker-config"}},
	})
}

// Generate serially before parallel subtests so scheduling cannot change the seed.
func TestConcurrentOverlappingTrees(t *testing.T) {
	require.Greater(t, *iterations, 0)
	t.Logf("replay: -trees.seed=%d -trees.iterations=%d", *seed, *iterations)
	for _, isolation := range []string{"none", "docker"} {
		t.Run(isolation, func(t *testing.T) {
			var f *harness.Fixture
			if isolation == "docker" {
				f = dockerFixture(t)
			} else {
				f = harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
			}
			c := f.Clients(harness.App)
			r := rand.New(rand.NewSource(*seed))
			for i := 0; i < *iterations; i++ {
				base := generate(r, 1+r.Intn(3))
				// Share all subtrees and most file blobs, but use different bytes at the
				// same path. This makes cross-action contamination observable.
				other := *base
				other.children = append([]*node(nil), base.children...)
				for j, child := range other.children {
					if child.name == "zero" {
						replacement := *child
						replacement.data = []byte(fmt.Sprintf("seed=%d sample=%d", *seed, i))
						other.children[j] = &replacement
					}
				}
				metadata := make([]*repb.ExecutedActionMetadata, 4)
				t.Run(fmt.Sprintf("seed_%d_batch_%d", *seed, i), func(t *testing.T) {
					for job := 0; job < 4; job++ {
						t.Run(fmt.Sprintf("job_%d", job), func(t *testing.T) {
							t.Parallel()
							ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
							defer cancel()
							n := base
							if job >= 2 {
								n = &other
							}
							instance := []string{"rand-trees/instance-a", "rand-trees/instance-b"}[job%2]
							// Different command digests prevent in-flight action deduplication.
							actionScript := fmt.Sprintf("# batch %d job %d\nsleep 2\n", i, job) + script
							metadata[job] = roundTrip(t, ctx, c, instance, n, isolation, job%2 == 0, actionScript, i)
						})
					}
				})
				if t.Failed() {
					return
				}
				overlaps := 0
				for a := 0; a < len(metadata); a++ {
					require.NotNil(t, metadata[a])
					require.NotNil(t, metadata[a].ExecutionStartTimestamp)
					require.NotNil(t, metadata[a].ExecutionCompletedTimestamp)
					for b := 0; b < a; b++ {
						if metadata[a].ExecutionStartTimestamp.AsTime().Before(metadata[b].ExecutionCompletedTimestamp.AsTime()) && metadata[b].ExecutionStartTimestamp.AsTime().Before(metadata[a].ExecutionCompletedTimestamp.AsTime()) {
							overlaps++
						}
					}
				}
				require.Positive(t, overlaps, "batch %d did not execute concurrently", i)
				t.Logf("batch %d: %d overlapping execution pairs", i, overlaps)
			}
		})
	}
}
