package readthrough_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

// Resolve only a fixture-owned container. This workaround can be removed when
// fixture lifecycle/command controls land in main.
func fixtureDocker(t *testing.T, ctx context.Context, container string) func(...string) ([]byte, error) {
	t.Helper()
	sockets, _ := filepath.Glob("/tmp/bbval-docker-*/docker.sock")
	hosts := []string{os.Getenv("DOCKER_HOST")}
	for _, socket := range sockets {
		hosts = append(hosts, "unix://"+socket)
	}
	for _, host := range hosts {
		prefix := []string{}
		if host != "" {
			prefix = append(prefix, "--host", host)
		}
		if exec.CommandContext(ctx, "docker", append(append([]string{}, prefix...), "inspect", container)...).Run() != nil {
			continue
		}
		t.Setenv("DOCKER_HOST", host)
		return func(args ...string) ([]byte, error) {
			commandCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return exec.CommandContext(commandCtx, "docker", append(append([]string{}, prefix...), args...)...).CombinedOutput()
		}
	}
	t.Fatal("fixture Docker socket not found")
	return nil
}

func cdcFixture(t *testing.T) (*harness.Fixture, *harness.Fixture, *harness.Fixture, context.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	app := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--cache.avg_chunk_size_bytes=65536"}}})
	docker := fixtureDocker(t, ctx, app.Endpoint(harness.App).Container)
	flagName := app.ID + "-read-through-flagd"
	configPath := filepath.Join(t.TempDir(), "flags.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"flags":{"cache_proxy.intercept_and_chunk_large_writes":{"state":"ENABLED","defaultVariant":"on","variants":{"on":true}},"cache_proxy.cdc_read_fast_path":{"state":"ENABLED","defaultVariant":"on","variants":{"on":true}}}}`), 0644))
	// v0.12.4, pinned multi-platform manifest. This sidecar serves only local flags.
	image := "ghcr.io/open-feature/flagd@sha256:186f35094d0493b4149e8c8f1a7413e2d1a0336622e009d0a2dbeca30c14aa89"
	t.Cleanup(func() {
		b, _ := docker("logs", flagName)
		_ = os.WriteFile(filepath.Join(app.OutputDir, "read-through-flagd.logs.txt"), b, 0644)
		b, err := docker("rm", "-f", flagName)
		require.NoError(t, err, string(b))
	})
	out, err := docker("run", "-d", "--name", flagName, "--network", app.Network, "--mount", "type=bind,source="+configPath+",target=/flags.json,readonly", image, "start", "--uri", "file:/flags.json")
	require.NoError(t, err, string(out))
	startProxy := func() *harness.Fixture {
		f := harness.Start(t, harness.Options{Components: []harness.Component{harness.CacheProxy}, AppTarget: app.Endpoint(harness.App).InternalGRPC, SkipAuthBootstrap: true,
			DockerArgs: map[harness.Component][]string{harness.CacheProxy: {"--network", app.Network, "-p", "127.0.0.1::9090"}},
			Args:       map[harness.Component][]string{harness.CacheProxy: {"--cache.avg_chunk_size_bytes=65536", "--experiments.flagd_backend=" + flagName + ":8015"}},
		})
		f.APIKey = app.APIKey
		return f
	}
	writer := startProxy()
	reader := startProxy()
	return app, writer, reader, ctx
}
