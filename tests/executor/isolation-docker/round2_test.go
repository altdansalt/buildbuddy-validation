package isolation_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

func docker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	require.NoError(t, err, "%v: %s", args, b)
	return strings.TrimSpace(string(b))
}

// Registry, credentials and pushed images belong only to this private daemon.
func localRegistry(t *testing.T, f *harness.Fixture) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	docker(t, "pull", "httpd:2.4.62-alpine")
	hash := docker(t, "run", "--rm", "httpd:2.4.62-alpine", "htpasswd", "-Bbn", "fixture", "disposable-password")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "htpasswd"), []byte(hash+"\n"), 0644))
	docker(t, "run", "-d", "--name", "round2-registry", "--network", f.Network,
		"-v", dir+":/auth:ro", "-e", "REGISTRY_AUTH=htpasswd", "-e", "REGISTRY_AUTH_HTPASSWD_REALM=fixture",
		"-e", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd", "registry:2.8.3")
	t.Cleanup(func() {
		if out := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); out != "" {
			logs := docker(t, "logs", "round2-registry")
			require.NoError(t, os.WriteFile(filepath.Join(out, "round2-registry.log"), []byte(logs), 0600))
		}
		docker(t, "rm", "-f", "-v", "round2-registry")
	})
	ip := docker(t, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "round2-registry")
	registry := ip + ":5000"
	// Login config is private to this test and never mounted into the executor.
	config := t.TempDir()
	deadline := time.Now().Add(30 * time.Second)
	for {
		cmd := exec.Command("docker", "--config", config, "login", registry, "-u", "fixture", "--password-stdin")
		cmd.Stdin = strings.NewReader("disposable-password\n")
		b, err := cmd.CombinedOutput()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry readiness/login: %s", b)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return registry, map[string]string{"container-registry-username": "fixture", "container-registry-password": "disposable-password", "test-registry-config": config}
}

func pushVariant(t *testing.T, registry, config, version string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine:3.21.3\nRUN echo "+version+" > /image-version && addgroup -g 23456 namedgroup && adduser -D -u 12345 -G namedgroup nameduser\n"), 0644))
	image := registry + "/conformance:mutable"
	docker(t, "build", "--network=none", "-t", image, dir)
	docker(t, "--config", config, "push", image)
	digest := docker(t, "image", "inspect", image, "--format", "{{index .RepoDigests 0}}")
	// Force the executor to pull, rather than inheriting our locally built tag.
	docker(t, "image", "rm", image)
	return digest
}

func TestDockerRound2(t *testing.T) {
	f := fixture(t)
	t.Run("registry_named_user", func(t *testing.T) { registryAndNamedUser(t, f) })
	t.Run("concurrent_isolation", func(t *testing.T) { concurrentIsolation(t, f) })
	t.Run("timeout_cancellation", func(t *testing.T) { timeoutCancellation(t, f) })
	t.Run("runtime_availability", func(t *testing.T) { runtimeAvailability(t, f) })
}

func registryAndNamedUser(t *testing.T, f *harness.Fixture) {
	registry, props := localRegistry(t, f)
	config := props["test-registry-config"]
	delete(props, "test-registry-config")
	first := pushVariant(t, registry, config, "first")
	image := registry + "/conformance:mutable"
	t.Run("credentials_required", func(t *testing.T) {
		r := execute(t, f, image, "echo must-not-run", "", nil)
		require.NotZero(t, r.GetStatus().GetCode())
		require.Empty(t, stdout(t, f, r))
	})
	t.Run("authenticated_pull_named_user", func(t *testing.T) {
		p := map[string]string{}
		for k, v := range props {
			p[k] = v
		}
		p["dockerUser"] = "nameduser:namedgroup"
		r := execute(t, f, image, "id -u; id -g; cat /image-version; cat sub/input.txt", "", p)
		success(t, r)
		require.Equal(t, "12345\n23456\nfirst\nmounted input\n", stdout(t, f, r))
	})
	second := pushVariant(t, registry, config, "second")
	require.NotEqual(t, first, second)
	t.Logf("workload digest before=%s after=%s", first, second)
	// Publishing uses the same daemon. Restore the executor's prior local tag to
	// model a remote registry update without changing its cached image.
	docker(t, "tag", first, image)
	cached := execute(t, f, image, "cat /image-version", "", props)
	success(t, cached)
	observed := stdout(t, f, cached)
	require.Contains(t, []string{"first\n", "second\n"}, observed)
	t.Logf("mutable tag before explicit eviction: %q (cache authorization TTL may retain first)", observed)
	docker(t, "image", "rm", image)

	// Immutable references provide a reproducible oracle across a tag update.
	for _, tc := range []struct{ ref, want string }{{first, "first\n"}, {second, "second\n"}, {image, "second\n"}} {
		r := execute(t, f, tc.ref, "cat /image-version", "", props)
		success(t, r)
		require.Equal(t, tc.want, stdout(t, f, r))
	}
	t.Run("wrong_credentials_after_cache", func(t *testing.T) {
		r := execute(t, f, image, "echo must-not-run", "", map[string]string{"container-registry-username": "fixture", "container-registry-password": "wrong"})
		require.NotZero(t, r.GetStatus().GetCode())
		require.Empty(t, stdout(t, f, r))
	})
}

func concurrentIsolation(t *testing.T, f *harness.Fixture) {
	const seed int64 = 20261012
	t.Logf("seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < 4; i++ {
		marker := fmt.Sprintf("%d-%016x", i, rng.Uint64())
		t.Run(marker, func(t *testing.T) {
			t.Parallel()
			script := fmt.Sprintf("set -eu; test ! -e /tmp/concurrent-marker; echo '%s' > /tmp/concurrent-marker; sleep 2; cat /tmp/concurrent-marker; cat sub/input.txt", marker)
			r := execute(t, f, "alpine:3.21.3", script, "", nil)
			success(t, r)
			require.Equal(t, marker+"\nmounted input\n", stdout(t, f, r))
		})
	}
}

func timeoutCancellation(t *testing.T, f *harness.Fixture) {
	// Pre-pull so the deadline tests a running process rather than registry latency.
	success(t, execute(t, f, "alpine:3.21.3", "echo ready", "", nil))
	r := executeTimeout(t, f, "alpine:3.21.3", `echo started; sleep 120 & wait`, "", nil, 3*time.Second)
	require.EqualValues(t, 4, r.GetStatus().GetCode(), "%v", r.GetStatus())
	require.Contains(t, stdout(t, f, r), "started")
	deadline := time.Now().Add(20 * time.Second)
	for {
		remaining := docker(t, "ps", "-a", "--filter", "ancestor=alpine:3.21.3", "--format", "{{.ID}} {{.Status}}")
		if remaining == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled action container remains: %s", remaining)
		}
		time.Sleep(200 * time.Millisecond)
	}
	r = execute(t, f, "alpine:3.21.3", "echo recovered", "", nil)
	success(t, r)
	require.Equal(t, "recovered\n", stdout(t, f, r))
}

func runtimeAvailability(t *testing.T, f *harness.Fixture) {
	// Inspect the released image instead of installing runtimes into it, which
	// would change the product under test. Record the shipped runtime versions.
	for _, binary := range []string{"podman", "runc", "crun"} {
		t.Run(binary, func(t *testing.T) {
			b, err := exec.Command("docker", "exec", f.Endpoint(harness.Executor).Container, "/bin/sh", "-c", "command -v "+binary+" && "+binary+" --version").CombinedOutput()
			if err != nil {
				t.Skipf("released executor runtime unavailable: %s: %s", binary, b)
			}
			t.Logf("released runtime: %s", b)
		})
	}
}

func TestAlternativeIsolationProviders(t *testing.T) {
	for _, provider := range []string{"podman", "oci"} {
		t.Run(provider, func(t *testing.T) {
			f := providerFixture(t, provider)
			executor := f.Endpoint(harness.Executor).Container
			// Share only the executor's network namespace: image pulls stay local and
			// need no outbound networking from this nested privileged fixture.
			name := "provider-registry"
			docker(t, "run", "-d", "--name", name, "--network", "container:"+executor, "registry:2.8.3")
			t.Cleanup(func() {
				if out := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); out != "" {
					require.NoError(t, os.WriteFile(filepath.Join(out, provider+"-registry.log"), []byte(docker(t, "logs", name)), 0600))
				}
				docker(t, "rm", "-f", "-v", name)
			})
			ip := docker(t, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", executor)
			deadline := time.Now().Add(20 * time.Second)
			for {
				b := docker(t, "logs", name)
				if strings.Contains(b, "listening on") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("local registry readiness: %s", b)
				}
				time.Sleep(200 * time.Millisecond)
			}
			docker(t, "pull", "alpine:3.21.3")
			image := ip + ":5000/provider:workload"
			docker(t, "tag", "alpine:3.21.3", image)
			docker(t, "push", image)
			if provider == "podman" {
				docker(t, "exec", executor, "/bin/sh", "-c", `mkdir -p /etc/containers/registries.conf.d; printf '[[registry]]\nlocation = "localhost:5000"\ninsecure = true\n' > /etc/containers/registries.conf.d/fixture.conf`)
			}
			props := map[string]string{"workload-isolation-type": provider, "dockerNetwork": "off"}
			r := execute(t, f, "localhost:5000/provider:workload", "cat /etc/alpine-release; cat sub/input.txt; echo dirty > /tmp/provider-marker", "", props)
			success(t, r)
			require.Equal(t, "3.21.3\nmounted input\n", stdout(t, f, r))
			r = execute(t, f, "localhost:5000/provider:workload", "test ! -e /tmp/provider-marker && echo clean", "", props)
			success(t, r)
			require.Equal(t, "clean\n", stdout(t, f, r))
		})
	}
}
