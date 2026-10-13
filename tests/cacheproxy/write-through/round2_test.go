package writethrough_test

import (
	"bytes"
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
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fixtureCommand(t *testing.T, host string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", append([]string{"--host", host}, args...)...).CombinedOutput()
	require.NoError(t, err, "docker %v: %s", args, out)
	return out
}

func randomBlob(seed int64, size int) []byte {
	data := make([]byte, size)
	_, _ = rand.New(rand.NewSource(seed)).Read(data)
	return data
}

// Exhaust only the external blob filesystem, leaving Pebble's WAL usable.
// Filling the runner filesystem would interfere with unrelated fixtures.
func TestLocalStorageFailures(t *testing.T) {
	for _, failure := range []string{"full", "readonly"} {
		t.Run(failure, func(t *testing.T) {
			mount := "/data/cache/blobs:rw,size=8m"
			if failure == "readonly" {
				mount = "/data/cache/blobs:ro,size=8m"
			}
			f := harness.Start(t, harness.Options{
				Components: []harness.Component{harness.App, harness.CacheProxy},
				DockerArgs: map[harness.Component][]string{harness.CacheProxy: {"--tmpfs", mount}},
				Args:       map[harness.Component][]string{harness.CacheProxy: {"--cache.pebble.min_bytes_auto_zstd_compression=9223372036854775807"}},
			})
			host := fixtureDockerHost(t, f.Endpoint(harness.CacheProxy).Container)
			if failure == "full" {
				busybox := filepath.Join(t.TempDir(), "busybox")
				loader := filepath.Join(t.TempDir(), "ld-musl-x86_64.so.1")
				fixtureCommand(t, host, "cp", "-L", f.ID+"-redis:/lib/ld-musl-x86_64.so.1", loader)
				fixtureCommand(t, host, "cp", loader, f.Endpoint(harness.CacheProxy).Container+":/lib/ld-musl-x86_64.so.1")
				fixtureCommand(t, host, "cp", f.ID+"-redis:/bin/busybox", busybox)
				fixtureCommand(t, host, "cp", busybox, f.Endpoint(harness.CacheProxy).Container+":/tmp/busybox")
				// dd must fail with ENOSPC, and an independent one-byte write must fail too.
				out := fixtureCommand(t, host, "exec", f.Endpoint(harness.CacheProxy).Container, "/tmp/busybox", "sh", "-c", "/tmp/busybox dd if=/dev/zero of=/data/cache/blobs/fill bs=1048576 count=9 2>&1; test $? -ne 0 && ! printf x > /data/cache/blobs/probe")
				require.Contains(t, string(out), "No space left on device")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			proxy, app := f.Clients(harness.CacheProxy), f.Clients(harness.App)
			for i, method := range []string{"batch", "stream", "compressed"} {
				data := randomBlob(int64(i+10), 256<<10)
				d := harness.Digest(data)
				var err error
				if method == "batch" {
					_, err = proxy.Upload(ctx, "storage-"+failure, data)
				} else {
					_, err = proxy.Write(ctx, "storage-"+failure, data, method == "compressed")
				}
				require.NoError(t, err, "local cache failures must preserve backend write-through")
				for _, c := range []*harness.Clients{app, proxy} {
					got, err := c.Read(ctx, "storage-"+failure, d)
					require.NoError(t, err)
					require.True(t, bytes.Equal(data, got))
				}
			}
			exerciseIncompleteWrites(t, proxy, app, "storage-aborts-"+failure)
		})
	}
}

func TestLimitsAndCancelledWrites(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.CacheProxy},
		Config:     map[harness.Component]harness.Config{harness.CacheProxy: {"cache": harness.Config{"max_size_bytes": int64(1 << 20)}}},
	})
	exerciseIncompleteWrites(t, f.Clients(harness.CacheProxy), f.Clients(harness.App), "limits")
}

func exerciseIncompleteWrites(t *testing.T, proxy, app *harness.Clients, instance string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// cache.max_size_bytes is an eviction budget, not a per-upload rejection limit.
	for i, compressed := range []bool{false, true} {
		data := randomBlob(int64(123+i), 5<<20)
		d, err := proxy.Write(ctx, instance, data, compressed)
		require.NoError(t, err)
		for _, c := range []*harness.Clients{app, proxy} {
			got, err := c.Read(ctx, instance, d)
			require.NoError(t, err)
			require.True(t, bytes.Equal(data, got))
		}
	}
	for _, mode := range []string{"cancel", "eof", "premature_finish", "overrun", "compressed_cancel", "compressed_eof", "compressed_premature_finish"} {
		t.Run(mode, func(t *testing.T) {
			data := randomBlob(int64(len(mode)), 6<<20)
			d := harness.Digest(data)
			writeCtx, stop := context.WithCancel(ctx)
			defer stop()
			stream, err := proxy.ByteStream.Write(writeCtx)
			require.NoError(t, err)
			compressed := strings.HasPrefix(mode, "compressed_")
			wire := data
			if compressed {
				wire, err = harness.Compress(data)
				require.NoError(t, err)
			}
			resource := harness.UploadResourceName(instance, d, compressed)
			for offset := 0; offset < 3<<20; offset += 256 << 10 {
				require.NoError(t, stream.Send(&bspb.WriteRequest{ResourceName: resource, WriteOffset: int64(offset), Data: wire[offset : offset+256<<10]}))
			}
			switch strings.TrimPrefix(mode, "compressed_") {
			case "cancel":
				stop()
			case "premature_finish":
				require.NoError(t, stream.Send(&bspb.WriteRequest{WriteOffset: 3 << 20, FinishWrite: true}))
			case "overrun":
				for offset := 3 << 20; offset < len(data); offset += 256 << 10 {
					require.NoError(t, stream.Send(&bspb.WriteRequest{WriteOffset: int64(offset), Data: wire[offset : offset+256<<10]}))
				}
				require.NoError(t, stream.Send(&bspb.WriteRequest{WriteOffset: int64(len(data)), Data: []byte("extra"), FinishWrite: true}))
			}
			_, err = stream.CloseAndRecv()
			require.Error(t, err, "unfinished or oversized stream must fail")
			// Recheck after server-side cancellation cleanup; any readable prefix fails.
			for attempt := 0; attempt < 3; attempt++ {
				for _, c := range []*harness.Clients{app, proxy} {
					_, err := c.Read(ctx, instance, d)
					require.Equal(t, codes.NotFound, status.Code(err), fmt.Sprintf("%s attempt %d", mode, attempt))
				}
				time.Sleep(100 * time.Millisecond)
			}
			// The same digest must remain writable after an aborted upload.
			_, err = proxy.Write(ctx, instance, data, false)
			require.NoError(t, err)
			for _, c := range []*harness.Clients{app, proxy} {
				got, err := c.Read(ctx, instance, d)
				require.NoError(t, err)
				require.True(t, bytes.Equal(data, got))
			}
		})
	}
}

// flagd is a test-local sidecar; no shared experiment service is contacted.
func TestFeatureFlaggedChunking(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.CacheProxy},
		Args: map[harness.Component][]string{
			harness.App:        {"--cache.avg_chunk_size_bytes=65536"},
			harness.CacheProxy: {"--cache.avg_chunk_size_bytes=65536"},
		},
	})
	container := f.Endpoint(harness.CacheProxy).Container
	host := fixtureDockerHost(t, container)
	name := f.ID + "-write-through-flagd"
	fixtureCommand(t, host, "create", "--name", name, "--network", f.Network, "--network-alias", "write-through-flagd", "ghcr.io/open-feature/flagd:v0.12.4", "start", "--uri", "file:/flags.json")
	t.Cleanup(func() { fixtureCommand(t, host, "rm", "-f", name) })
	flags := filepath.Join(f.OutputDir, "round2-flags.json")
	require.NoError(t, os.WriteFile(flags, []byte(`{"flags":{"cache_proxy.intercept_and_chunk_large_writes":{"state":"ENABLED","variants":{"on":true},"defaultVariant":"on"}}}`), 0644))
	fixtureCommand(t, host, "cp", flags, name+":/flags.json")
	fixtureCommand(t, host, "start", name)
	config, err := os.ReadFile(filepath.Join(f.OutputDir, "cacheproxy.yaml"))
	require.NoError(t, err)
	config = append(config, []byte("\nexperiments:\n  flagd_backend: write-through-flagd:8015\n")...)
	path := filepath.Join(f.OutputDir, "round2-proxy.yaml")
	require.NoError(t, os.WriteFile(path, config, 0644))
	fixtureCommand(t, host, "cp", path, container+":/config.yaml")
	fixtureCommand(t, host, "restart", container)
	address := strings.TrimSpace(string(fixtureCommand(t, host, "port", container, "1985/tcp")))
	proxy, app := harness.Dial(t, address, f.APIKey), f.Clients(harness.App)
	t.Cleanup(func() {
		logs := fixtureCommand(t, host, "logs", name)
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, "round2-flagd.log"), logs, 0644))
		metadata := fixtureCommand(t, host, "inspect", name)
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, "round2-flagd.inspect.json"), metadata, 0644))
		if t.Failed() {
			t.Logf("flagd logs: %s", logs)
			t.Logf("proxy logs: %s", fixtureCommand(t, host, "logs", container))
		}
	})
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := proxy.Upload(ctx, "chunk-ready", []byte("ready"))
		return err == nil
	}, time.Minute, time.Second)
	exerciseIncompleteWrites(t, proxy, app, "chunked")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	split, err := app.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{InstanceName: "chunked", BlobDigest: harness.Digest(randomBlob(123, 5<<20)), DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Greater(t, len(split.GetChunkDigests()), 1, "feature-enabled upload must produce multiple chunks")
	// Preserve evidence that the enabled feature path actually ran.
	logs := fixtureCommand(t, host, "logs", container)
	require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, "round2-chunking.log"), logs, 0644))
}

func TestReceiveMessageLimit(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.CacheProxy},
		Args:       map[harness.Component][]string{harness.CacheProxy: {"--grpc_max_recv_msg_size_bytes=1100000", "--app.grpc_max_recv_msg_size_bytes=1100000"}},
	})
	proxy, app := f.Clients(harness.CacheProxy), f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, method := range []string{"batch", "stream"} {
		data := randomBlob(int64(len(method)+99), 2<<20)
		d := harness.Digest(data)
		var err error
		if method == "batch" {
			_, err = proxy.Upload(ctx, "message-limit", data)
		} else {
			stream, e := proxy.ByteStream.Write(ctx)
			require.NoError(t, e)
			_ = stream.Send(&bspb.WriteRequest{ResourceName: harness.UploadResourceName("message-limit", d, false), Data: data, FinishWrite: true})
			_, err = stream.CloseAndRecv()
		}
		require.Equal(t, codes.ResourceExhausted, status.Code(err))
		for _, c := range []*harness.Clients{app, proxy} {
			_, err := c.Read(ctx, "message-limit", d)
			require.Equal(t, codes.NotFound, status.Code(err))
		}
		// The payload itself is permitted when split into bounded messages.
		_, err = proxy.Write(ctx, "message-limit", data, false)
		require.NoError(t, err)
		for _, c := range []*harness.Clients{app, proxy} {
			got, err := c.Read(ctx, "message-limit", d)
			require.NoError(t, err)
			require.True(t, bytes.Equal(data, got))
		}
	}
}
