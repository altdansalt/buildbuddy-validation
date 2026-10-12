package storagegc_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fixture(t *testing.T, v2 bool, size int64) *harness.Fixture {
	return harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{
		harness.App: {"cache": harness.Config{"max_size_bytes": size, "pebble": harness.Config{"root_directory": ""}, "disk": harness.Config{"root_directory": "/data/disk-cache", "use_v2_layout": v2}}},
	}})
}

// No writes occur during this check, so a missing result must agree across APIs.
func checkBlob(t *testing.T, ctx context.Context, c *harness.Clients, d *repb.Digest, data []byte, missing bool) {
	t.Helper()
	r, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Equal(t, missing, len(r.MissingBlobDigests) == 1)
	b, err := c.Read(ctx, "", d)
	if missing {
		require.Equal(t, codes.NotFound, status.Code(err))
	} else {
		require.NoError(t, err)
		require.Equal(t, data, b)
	}
	batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Len(t, batch.Responses, 1)
	if missing {
		require.Equal(t, int32(codes.NotFound), batch.Responses[0].GetStatus().GetCode())
	} else {
		require.Zero(t, batch.Responses[0].GetStatus().GetCode())
		require.Equal(t, data, batch.Responses[0].Data)
	}
}

func TestEvictionAndReupload(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%t", v2), func(t *testing.T) {
			f := fixture(t, v2, 256<<10)
			c := f.Clients(harness.App)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var digests []*repb.Digest
			var blobs [][]byte
			for i := 0; i < 12; i++ {
				b := bytes.Repeat([]byte{byte(i + 1)}, 64<<10)
				d, err := c.Write(ctx, "", b, false)
				require.NoError(t, err)
				digests = append(digests, d)
				blobs = append(blobs, b)
			}
			// Source janitor checks every 100ms. Wait for settled pressure, without reads
			// refreshing LRU order while writes are in flight.
			time.Sleep(time.Second)
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: digests, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(missing.MissingBlobDigests), 8, "256 KiB limit must retain at most four 64 KiB blobs")
			gone := map[string]bool{}
			for _, d := range missing.MissingBlobDigests {
				gone[d.Hash] = true
			}
			for i, d := range digests {
				checkBlob(t, ctx, c, d, blobs[i], gone[d.Hash])
			}
			// A previously evicted digest can be uploaded again and read correctly.
			d := missing.MissingBlobDigests[0]
			for i, candidate := range digests {
				if candidate.Hash == d.Hash {
					_, err := c.Upload(ctx, "", blobs[i])
					require.NoError(t, err)
					checkBlob(t, ctx, c, d, blobs[i], false)
				}
			}
		})
	}
}

// Discover the fixture's private daemon without reaching outside its containers.
// The harness currently does not expose Docker lifecycle methods.
func docker(t *testing.T, f *harness.Fixture, args ...string) []byte {
	t.Helper()
	sockets, err := filepath.Glob("/tmp/bbval-docker-*/docker.sock")
	require.NoError(t, err)
	hosts := append([]string{""}, sockets...)
	for _, host := range hosts {
		prefix := []string{}
		if host != "" {
			prefix = []string{"--host", "unix://" + host}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := exec.CommandContext(ctx, "docker", append(prefix, "inspect", f.Endpoint(harness.App).Container)...).CombinedOutput()
		cancel()
		if err != nil {
			continue
		}
		ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", append(prefix, args...)...).CombinedOutput()
		require.NoError(t, err, "%s", out)
		return out
	}
	t.Fatal("fixture Docker daemon not found")
	return nil
}

func TestCrashPersistenceAndPartialWrite(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%t", v2), func(t *testing.T) {
			f := fixture(t, v2, 16<<20)
			c := f.Clients(harness.App)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			data := bytes.Repeat([]byte("committed"), 8192)
			d, err := c.Write(ctx, "", data, false)
			require.NoError(t, err)
			partial := bytes.Repeat([]byte("unfinished"), 65536)
			pd := harness.Digest(partial)
			stream, err := c.ByteStream.Write(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&bspb.WriteRequest{ResourceName: harness.UploadResourceName("", pd, false), Data: partial[:len(partial)/2]}))
			// Give the server time to process the uncommitted stream before SIGKILL.
			time.Sleep(time.Second)
			checkBlob(t, ctx, c, pd, nil, true)
			name := f.Endpoint(harness.App).Container
			docker(t, f, "kill", "--signal=KILL", name)
			_, _ = stream.CloseAndRecv()
			docker(t, f, "start", name)
			client := &http.Client{Timeout: 2 * time.Second}
			require.Eventually(t, func() bool {
				r, err := client.Get(f.Endpoint(harness.App).HTTP + "/readyz?server-type=app")
				if err != nil {
					return false
				}
				defer r.Body.Close()
				return r.StatusCode == 200
			}, time.Minute, 200*time.Millisecond)
			c = f.Clients(harness.App)
			checkBlob(t, ctx, c, d, data, false)
			checkBlob(t, ctx, c, pd, nil, true)
			_, err = c.Write(ctx, "", partial, false)
			require.NoError(t, err)
			checkBlob(t, ctx, c, pd, partial, false)
			// Also verify a graceful restart preserves both committed entries.
			docker(t, f, "restart", "--time=20", name)
			require.Eventually(t, func() bool {
				r, err := client.Get(f.Endpoint(harness.App).HTTP + "/readyz?server-type=app")
				if err != nil {
					return false
				}
				defer r.Body.Close()
				return r.StatusCode == 200
			}, time.Minute, 200*time.Millisecond)
			c = f.Clients(harness.App)
			checkBlob(t, ctx, c, d, data, false)
			checkBlob(t, ctx, c, pd, partial, false)
		})
	}
}

// Eviction may race a reader. A read can report a miss, but must never return
// truncated or different content, or turn an ordinary miss into INTERNAL.
func TestConcurrentReadsDuringEviction(t *testing.T) {
	f := fixture(t, true, 256<<10)
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	errors := make(chan error, 3)
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				data := bytes.Repeat([]byte{byte(1 + worker*30 + i)}, 64<<10)
				d, err := c.Write(ctx, "", data, false)
				if err != nil {
					errors <- fmt.Errorf("write worker %d: %w", worker, err)
					return
				}
				b, err := c.Read(ctx, "", d)
				if err != nil && status.Code(err) != codes.NotFound {
					errors <- fmt.Errorf("read worker %d: %w", worker, err)
					return
				}
				if err == nil && !bytes.Equal(b, data) {
					errors <- fmt.Errorf("read worker %d: corrupted or truncated bytes", worker)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
}
