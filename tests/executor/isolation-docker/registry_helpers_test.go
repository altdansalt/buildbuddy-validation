package isolation_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
