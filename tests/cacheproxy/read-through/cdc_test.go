package readthrough_test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	hosts := []string{""}
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

func TestCDCSeededReadThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
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
	wc, rc := writer.Clients(harness.CacheProxy), reader.Clients(harness.CacheProxy)
	rng := rand.New(rand.NewSource(20261013))
	t.Log("CDC replay seed: 20261013")
	for caseID := 0; caseID < 3; caseID++ {
		data := make([]byte, (1<<20)+rng.Intn(1<<20))
		rng.Read(data)
		d, err := wc.Write(ctx, instance, data, caseID%2 == 1)
		require.NoError(t, err)
		for pass := 0; pass < 2; pass++ {
			got, err := rc.Read(ctx, instance, d)
			require.NoError(t, err)
			require.Equal(t, data, got)
			offset := int64(rng.Intn(len(data)))
			limit := int64(1 + rng.Intn(100000))
			end := offset + limit
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			got, err = read(ctx, rc, d, offset, limit, false)
			require.NoError(t, err)
			require.Equal(t, data[offset:end], got)
			compressed, err := read(ctx, rc, d, offset, 0, true)
			require.NoError(t, err)
			got, err = harness.Decompress(compressed)
			require.NoError(t, err)
			require.Equal(t, data[offset:], got)
		}
	}
	// Prove enabled paths actually ran, rather than accepting ordinary-read fallback.
	checkMetric := func(f *harness.Fixture, needle string) {
		out, err := docker("port", f.Endpoint(harness.CacheProxy).Container, "9090")
		require.NoError(t, err, string(out))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+strings.TrimSpace(string(out))+"/metrics", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)
		found := false
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, needle+"{") {
				fields := strings.Fields(line)
				v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
				require.NoError(t, err)
				if v > 0 {
					found = true
					t.Log(line)
				}
			}
		}
		require.True(t, found, fmt.Sprintf("no positive metric %s; CDC path not exercised", needle))
	}
	checkMetric(writer, "buildbuddy_proxy_byte_stream_chunked_write_chunks_total")
	checkMetric(reader, "buildbuddy_proxy_byte_stream_chunked_read_chunks_remote")
}
