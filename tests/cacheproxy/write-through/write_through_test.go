package writethrough_test

import (
	"bytes"
	"context"
	"fmt"
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

// The v2.314.0 CAS and ByteStream proxies return the remote response only after
// the remote write completes; local cache population is best effort.
// https://github.com/buildbuddy-io/buildbuddy/tree/v2.314.0/enterprise/server
// REAPI specifies independent BatchUpdateBlobs statuses and digest validation:
// https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto
func TestWriteThrough(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	proxy, app := f.Clients(harness.CacheProxy), f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for _, method := range []string{"batch", "stream", "compressed"} {
		t.Run(method, func(t *testing.T) {
			for _, size := range []int{0, 1, 2<<20 + 17} {
				t.Run(fmt.Sprint(size), func(t *testing.T) {
					data := bytes.Repeat([]byte{byte((size + len(method)) % 251)}, size)
					// Distinct instance names exercise routing, including nested names.
					instance := "write-through/" + method
					var d *repb.Digest
					var err error
					if method == "batch" {
						d, err = proxy.Upload(ctx, instance, data)
					} else {
						d, err = proxy.Write(ctx, instance, data, method == "compressed")
					}
					require.NoError(t, err)
					for _, c := range []*harness.Clients{app, proxy} {
						got, err := c.Read(ctx, instance, d)
						require.NoError(t, err, "successful writes must be immediately visible")
						require.True(t, bytes.Equal(data, got), "read bytes differ from uploaded bytes")
					}
				})
			}
		})
	}
	t.Run("concurrent_duplicates", func(t *testing.T) {
		data := bytes.Repeat([]byte("duplicate write\n"), 100000)
		start := make(chan struct{})
		errs := make(chan error, 12)
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				var err error
				if i%3 == 0 {
					_, err = proxy.Upload(ctx, "duplicates", data)
				} else {
					_, err = proxy.Write(ctx, "duplicates", data, i%3 == 2)
				}
				errs <- err
			}(i)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		for _, c := range []*harness.Clients{proxy, app} {
			got, err := c.Read(ctx, "duplicates", harness.Digest(data))
			require.NoError(t, err)
			require.True(t, bytes.Equal(data, got), "read bytes differ from uploaded bytes")
		}
	})
	t.Run("mixed_batch_validation", func(t *testing.T) {
		good, bad := []byte("valid batch member"), []byte("incorrect bytes")
		gd, bd := harness.Digest(good), harness.Digest([]byte("expected content"))
		res, err := proxy.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "mixed", DigestFunction: repb.DigestFunction_SHA256, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: gd, Data: good}, {Digest: bd, Data: bad}}})
		require.NoError(t, err)
		require.Len(t, res.Responses, 2)
		statuses := map[string]int32{}
		for _, r := range res.Responses {
			statuses[r.Digest.Hash] = r.GetStatus().GetCode()
		}
		require.Equal(t, int32(codes.OK), statuses[gd.Hash])
		require.Equal(t, int32(codes.InvalidArgument), statuses[bd.Hash])
		for _, c := range []*harness.Clients{proxy, app} {
			got, err := c.Read(ctx, "mixed", gd)
			require.NoError(t, err)
			require.Equal(t, good, got)
			_, err = c.Read(ctx, "mixed", bd)
			require.Equal(t, codes.NotFound, status.Code(err))
		}
	})
	t.Run("stream_digest_validation", func(t *testing.T) {
		expected := harness.Digest([]byte("expected stream content"))
		stream, err := proxy.ByteStream.Write(ctx)
		require.NoError(t, err)
		err = stream.Send(&bspb.WriteRequest{ResourceName: harness.UploadResourceName("invalid-stream", expected, false), Data: []byte("incorrect stream bytes!"), FinishWrite: true})
		require.NoError(t, err)
		_, err = stream.CloseAndRecv()
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		for _, c := range []*harness.Clients{proxy, app} {
			_, err := c.Read(ctx, "invalid-stream", expected)
			require.Equal(t, codes.NotFound, status.Code(err), "invalid uploads must not populate either cache")
		}
	})
	t.Run("backend_unavailable", func(t *testing.T) {
		// Warm remote authentication before interrupting only the fixture backend.
		_, err := proxy.Upload(ctx, "failure", []byte("warm auth"))
		require.NoError(t, err)
		host := fixtureDockerHost(t, f.Endpoint(harness.App).Container)
		docker := func(verb string) {
			t.Helper()
			cctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			out, err := exec.CommandContext(cctx, "docker", "--host", host, verb, f.Endpoint(harness.App).Container).CombinedOutput()
			require.NoError(t, err, "%s: %s", verb, out)
		}
		docker("pause")
		paused := true
		defer func() {
			if paused {
				docker("unpause")
			}
		}()
		for _, method := range []string{"batch", "stream"} {
			data := []byte("backend interruption " + method)
			short, stop := context.WithTimeout(ctx, 3*time.Second)
			if method == "batch" {
				_, err = proxy.Upload(short, "failure", data)
			} else {
				_, err = proxy.Write(short, "failure", data, false)
			}
			stop()
			require.Error(t, err, "local cache must not conceal a backend failure")
			require.Contains(t, []codes.Code{codes.DeadlineExceeded, codes.Unavailable}, status.Code(err))
		}
		docker("unpause")
		paused = false
		// Retry after recovery and prove both paths commit at the real backend.
		for _, method := range []string{"batch", "stream"} {
			data := []byte("backend interruption " + method)
			var d *repb.Digest
			if method == "batch" {
				d, err = proxy.Upload(ctx, "failure", data)
			} else {
				d, err = proxy.Write(ctx, "failure", data, false)
			}
			require.NoError(t, err)
			got, err := app.Read(ctx, "failure", d)
			require.NoError(t, err)
			require.True(t, bytes.Equal(data, got), "read bytes differ from uploaded bytes")
		}
	})
}

// The harness's fallback daemon socket is private. Discover it by verifying
// ownership of this fixture's exact container, without touching other containers.
func fixtureDockerHost(t *testing.T, container string) string {
	t.Helper()
	sockets, err := filepath.Glob("/tmp/bbval-docker-*/docker.sock")
	require.NoError(t, err)
	hosts := []string{"unix:///var/run/docker.sock"}
	for _, socket := range sockets {
		hosts = append(hosts, "unix://"+socket)
	}
	for _, host := range hosts {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := exec.CommandContext(ctx, "docker", "--host", host, "inspect", container).Run()
		cancel()
		if err == nil {
			return host
		}
	}
	t.Fatal("could not locate fixture Docker daemon")
	return ""
}

// With local validation disabled, the remote backend is solely responsible for
// rejecting a corrupt stream. The proxy must not acknowledge or cache it before
// receiving the backend's validation result.
func TestBackendValidationBeforeLocalCommit(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.CacheProxy},
		Args:       map[harness.Component][]string{harness.CacheProxy: {"--cache_proxy.skip_write_validation=true"}},
	})
	proxy, app := f.Clients(harness.CacheProxy), f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprint(compressed), func(t *testing.T) {
			data := []byte(fmt.Sprintf("backend validated stream compressed=%t", compressed))
			d, err := proxy.Write(ctx, "remote-validation", data, compressed)
			require.NoError(t, err)
			for _, c := range []*harness.Clients{app, proxy} {
				got, err := c.Read(ctx, "remote-validation", d)
				require.NoError(t, err)
				require.Equal(t, data, got)
			}
			// Preserve size while changing content, isolating checksum validation.
			expected := harness.Digest(append([]byte("expected: "), data...))
			corrupt := append([]byte("corrupt!: "), data...)
			if compressed {
				corrupt, err = harness.Compress(corrupt)
				require.NoError(t, err)
			}
			stream, err := proxy.ByteStream.Write(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&bspb.WriteRequest{ResourceName: harness.UploadResourceName("remote-validation", expected, compressed), Data: corrupt, FinishWrite: true}))
			_, err = stream.CloseAndRecv()
			require.Equal(t, codes.InvalidArgument, status.Code(err), "backend validation error must reach client")
			for _, c := range []*harness.Clients{app, proxy} {
				_, err := c.Read(ctx, "remote-validation", expected)
				require.Equal(t, codes.NotFound, status.Code(err), "backend-rejected blob must not be cached locally")
			}
		})
	}
}
