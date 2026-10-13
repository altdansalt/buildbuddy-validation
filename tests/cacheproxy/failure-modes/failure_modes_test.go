package failure_modes_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const instance = "failure-modes"

// The harness owns a private daemon when no default daemon is running. Discover
// it by inspecting only our fixture container, never by mutating other resources.
func docker(t *testing.T, f *harness.Fixture) func(...string) string {
	t.Helper()
	sockets, err := filepath.Glob("/tmp/bbval-docker-*/docker.sock")
	require.NoError(t, err)
	hosts := append([]string{""}, sockets...)
	for _, host := range hosts {
		prefix := []string{}
		if host != "" {
			prefix = []string{"--host", "unix://" + host}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := exec.CommandContext(ctx, "docker", append(prefix, "inspect", f.Endpoint(harness.App).Container)...).Run()
		cancel()
		if err != nil {
			continue
		}
		return func(args ...string) string {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "docker", append(prefix, args...)...).CombinedOutput()
			require.NoError(t, err, "docker %v: %s", args, out)
			return strings.TrimSpace(string(out))
		}
	}
	t.Fatal("cannot locate fixture Docker daemon")
	return nil
}

func read(t *testing.T, c *harness.Clients, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := c.Read(ctx, instance, harness.Digest(data))
	require.NoError(t, err)
	require.Equal(t, data, got)
}

// Docker may reallocate randomly published ports when the same container starts.
func refreshPorts(t *testing.T, dkr func(...string) string, ep *harness.Endpoint) {
	t.Helper()
	var ports map[string][]struct{ HostIP, HostPort string }
	require.NoError(t, json.Unmarshal([]byte(dkr("inspect", "--format", "{{json .NetworkSettings.Ports}}", ep.Container)), &ports))
	require.Len(t, ports["8080/tcp"], 1)
	require.Len(t, ports["1985/tcp"], 1)
	ep.HTTP = "http://127.0.0.1:" + ports["8080/tcp"][0].HostPort
	ep.GRPC = "127.0.0.1:" + ports["1985/tcp"][0].HostPort
	t.Logf("restart ports: HTTP=%s gRPC=%s", ep.HTTP, ep.GRPC)
}

func ready(t *testing.T, ep *harness.Endpoint) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	require.Eventually(t, func() bool {
		r, err := client.Get(ep.HTTP + "/readyz?server-type=" + ep.ServerType)
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	}, time.Minute, 250*time.Millisecond)
}

func TestBackendFaultsAndRecovery(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	dkr := docker(t, f)
	proxy, app := f.Clients(harness.CacheProxy), f.Clients(harness.App)
	warm := []byte("warm cached bytes survive backend faults")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := proxy.Write(ctx, instance, warm, false)
	require.NoError(t, err)
	read(t, proxy, warm) // warm both data and remote authentication cache
	cold := []byte("backend-only bytes fetched after recovery")
	_, err = app.Write(ctx, instance, cold, false)
	require.NoError(t, err)
	backend := f.Endpoint(harness.App).Container
	for _, fault := range []string{"pause", "stop", "disconnect"} {
		t.Run(fault, func(t *testing.T) {
			restored := false
			restore := func() {
				if restored {
					return
				}
				restored = true
				switch fault {
				case "pause":
					dkr("unpause", backend)
				case "stop":
					dkr("start", backend)
					refreshPorts(t, dkr, f.Endpoint(harness.App))
					app = f.Clients(harness.App)
				case "disconnect":
					dkr("network", "connect", "--alias", "app", f.Network, backend)
					refreshPorts(t, dkr, f.Endpoint(harness.App))
					app = f.Clients(harness.App)
				}
			}
			t.Cleanup(restore)
			switch fault {
			case "pause":
				dkr("pause", backend)
			case "stop":
				dkr("stop", "--time", "1", backend)
			case "disconnect":
				dkr("network", "disconnect", f.Network, backend)
			}
			read(t, proxy, warm)
			batchCtx, batchCancel := context.WithTimeout(context.Background(), 5*time.Second)
			batch, batchErr := proxy.CAS.BatchReadBlobs(batchCtx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{harness.Digest(warm)}, DigestFunction: repb.DigestFunction_SHA256})
			batchCancel()
			require.NoError(t, batchErr)
			require.Len(t, batch.Responses, 1)
			require.Zero(t, batch.Responses[0].GetStatus().GetCode())
			require.Equal(t, warm, batch.Responses[0].Data)
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			_, err := proxy.Read(ctx, instance, harness.Digest([]byte("never cached "+fault)))
			cancel()
			require.Contains(t, []codes.Code{codes.Unavailable, codes.DeadlineExceeded}, status.Code(err), "cache miss during %s must fail without claiming NotFound", fault)
			ctx, cancel = context.WithTimeout(context.Background(), 1500*time.Millisecond)
			_, err = proxy.Write(ctx, instance, []byte("uncommitted write "+fault), false)
			cancel()
			require.Error(t, err, "must not acknowledge a write that cannot reach authoritative backend")
			restore()
			ready(t, f.Endpoint(harness.App))
			require.Eventually(t, func() bool {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				got, err := proxy.Read(ctx, instance, harness.Digest(cold))
				return err == nil && bytes.Equal(got, cold)
			}, 30*time.Second, 250*time.Millisecond)
			payload := []byte("successful write after " + fault)
			ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			_, err = proxy.Write(ctx, instance, payload, false)
			cancel()
			require.NoError(t, err)
			read(t, app, payload)
		})
	}
	logs := dkr("logs", f.Endpoint(harness.CacheProxy).Container)
	require.NotContains(t, logs, "panic:")
	require.NotContains(t, logs, "fatal error:")
}

func TestProxyRestartPersistsLocalCAS(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	dkr := docker(t, f)
	proxy := f.Clients(harness.CacheProxy)
	data := bytes.Repeat([]byte("persistent-CAS"), 10000)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := proxy.Write(ctx, instance, data, false)
	require.NoError(t, err)
	read(t, proxy, data)
	// Graceful shutdown flushes Pebble metadata (normal writes use NoSync).
	dkr("stop", "--time", "30", f.Endpoint(harness.CacheProxy).Container)
	dkr("start", f.Endpoint(harness.CacheProxy).Container)
	refreshPorts(t, dkr, f.Endpoint(harness.CacheProxy))
	ready(t, f.Endpoint(harness.CacheProxy))
	proxy = f.Clients(harness.CacheProxy)
	local := metadata.AppendToOutgoingContext(ctx, "proxy_skip_remote", "true")
	got, err := proxy.Read(local, instance, harness.Digest(data))
	require.NoError(t, err)
	require.Equal(t, data, got)
	read(t, f.Clients(harness.App), data)
	// SIGKILL may lose unsynced local metadata; the authoritative backend must
	// still supply acknowledged bytes without corruption after crash recovery.
	dkr("kill", "--signal", "KILL", f.Endpoint(harness.CacheProxy).Container)
	dkr("start", f.Endpoint(harness.CacheProxy).Container)
	refreshPorts(t, dkr, f.Endpoint(harness.CacheProxy))
	ready(t, f.Endpoint(harness.CacheProxy))
	read(t, f.Clients(harness.CacheProxy), data)
}

func TestLocalEvictionFallsBackToBackend(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.CacheProxy},
		Config:     map[harness.Component]harness.Config{harness.CacheProxy: {"cache": harness.Config{"max_size_bytes": int64(2 << 20)}}},
		Args:       map[harness.Component][]string{harness.CacheProxy: {"--cache.pebble.min_eviction_age=0s"}},
	})
	proxy := f.Clients(harness.CacheProxy)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var payloads [][]byte
	for i := 0; i < 32; i++ {
		b := make([]byte, 256<<10)
		_, err := rand.Read(b)
		require.NoError(t, err)
		payloads = append(payloads, b)
		_, err = proxy.Write(ctx, instance, b, false)
		require.NoError(t, err)
	}
	var evicted []byte
	require.Eventually(t, func() bool {
		for _, b := range payloads {
			probe, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(ctx, "proxy_skip_remote", "true"), time.Second)
			_, err := proxy.Read(probe, instance, harness.Digest(b))
			cancel()
			if status.Code(err) == codes.NotFound {
				evicted = b
				return true
			}
			if err != nil {
				t.Fatalf("local eviction probe: %v", err)
			}
		}
		return false
	}, time.Minute, time.Second, "small local cache must actually evict before fallback is tested")
	read(t, f.Clients(harness.App), evicted)
	read(t, proxy, evicted)
}
