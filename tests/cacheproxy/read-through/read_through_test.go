package readthrough_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const instance = "read-through"

func fixture(t *testing.T) (*harness.Fixture, context.Context) {
	t.Helper()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return f, ctx
}
func local(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "proxy_skip_remote", "true")
}
func read(ctx context.Context, c *harness.Clients, d *repb.Digest, offset, limit int64, compressed bool) ([]byte, error) {
	s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, compressed), ReadOffset: offset, ReadLimit: limit})
	if err != nil {
		return nil, err
	}
	var b []byte
	for {
		r, err := s.Recv()
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return b, err
		}
		b = append(b, r.Data...)
	}
}

func TestColdWarmAndPartialReads(t *testing.T) {
	f, ctx := fixture(t)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	data := bytes.Repeat([]byte("0123456789abcdef"), 200000)
	for _, tc := range []struct {
		name          string
		offset, limit int64
	}{{"prefix", 0, 17}, {"middle", 12345, 100001}, {"suffix", int64(len(data) - 31), 0}, {"eof", int64(len(data)), 0}} {
		t.Run(tc.name, func(t *testing.T) {
			payload := append(append([]byte(nil), data...), []byte(tc.name)...)
			d, err := app.Write(ctx, instance, payload, false)
			require.NoError(t, err)
			_, err = proxy.Read(local(ctx), instance, d)
			require.Equal(t, codes.NotFound, status.Code(err), "must start cold")
			got, err := read(ctx, proxy, d, tc.offset, tc.limit, false)
			require.NoError(t, err)
			end := int64(len(payload))
			if tc.limit > 0 && tc.offset+tc.limit < end {
				end = tc.offset + tc.limit
			}
			require.Equal(t, payload[tc.offset:end], got)
			// A partial fetch must never publish a truncated blob under the full digest.
			cached, err := proxy.Read(local(ctx), instance, d)
			if err == nil {
				require.Equal(t, payload, cached)
			} else {
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			got, err = proxy.Read(ctx, instance, d)
			require.NoError(t, err)
			require.Equal(t, payload, got)
			got, err = proxy.Read(local(ctx), instance, d)
			require.NoError(t, err)
			require.Equal(t, payload, got)
			got, err = read(ctx, proxy, d, tc.offset, tc.limit, false)
			require.NoError(t, err)
			require.Equal(t, payload[tc.offset:end], got)
		})
	}
}

func TestCompressedColdReadAndConcurrentMisses(t *testing.T) {
	f, ctx := fixture(t)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	data := bytes.Repeat([]byte("compressed read through data\n"), 10000)
	d, err := app.Write(ctx, instance, data, false)
	require.NoError(t, err)
	got, err := read(ctx, proxy, d, 0, 0, true)
	require.NoError(t, err)
	decoded, err := harness.Decompress(got)
	require.NoError(t, err)
	require.Equal(t, data, decoded)
	got, err = proxy.Read(local(ctx), instance, d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	data = append(data, []byte("concurrent")...)
	d, err = app.Write(ctx, instance, data, false)
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := proxy.Read(ctx, instance, d)
			if err == nil && !bytes.Equal(data, got) {
				err = fmt.Errorf("incorrect concurrent data")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	got, err = proxy.Read(local(ctx), instance, d)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestMixedBatchAndMissBecomesPresent(t *testing.T) {
	f, ctx := fixture(t)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	warm := []byte("warm batch")
	cold := []byte("cold batch")
	absent := []byte("later present")
	wd, err := app.Upload(ctx, instance, warm)
	require.NoError(t, err)
	_, err = proxy.Read(ctx, instance, wd)
	require.NoError(t, err)
	cd, err := app.Upload(ctx, instance, cold)
	require.NoError(t, err)
	ad := harness.Digest(absent)
	req := &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{wd, cd, ad}, DigestFunction: repb.DigestFunction_SHA256}
	for i := 0; i < 2; i++ {
		r, err := proxy.CAS.BatchReadBlobs(ctx, req)
		require.NoError(t, err)
		require.Len(t, r.Responses, 3)
		seen := map[string]bool{}
		for _, v := range r.Responses {
			require.False(t, seen[v.Digest.Hash])
			seen[v.Digest.Hash] = true
			switch v.Digest.Hash {
			case wd.Hash:
				require.Zero(t, v.GetStatus().GetCode())
				require.Equal(t, warm, v.Data)
			case cd.Hash:
				require.Zero(t, v.GetStatus().GetCode())
				require.Equal(t, cold, v.Data)
			case ad.Hash:
				require.EqualValues(t, codes.NotFound, v.GetStatus().GetCode())
			default:
				t.Fatalf("unexpected digest %v", v.Digest)
			}
		}
	}
	_, err = proxy.Read(ctx, instance, ad)
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = app.Upload(ctx, instance, absent)
	require.NoError(t, err)
	got, err := proxy.Read(ctx, instance, ad)
	require.NoError(t, err)
	require.Equal(t, absent, got)
}

func TestActionResultBackendUpdate(t *testing.T) {
	f, ctx := fixture(t)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	d := harness.Digest([]byte("action identity"))
	req := &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256}
	_, err := proxy.AC.GetActionResult(ctx, req)
	require.Equal(t, codes.NotFound, status.Code(err))
	for _, text := range []string{"first", "replacement", "third"} {
		_, err := app.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256, ActionResult: &repb.ActionResult{StdoutRaw: []byte(text)}})
		require.NoError(t, err)
		for i := 0; i < 2; i++ {
			r, err := proxy.AC.GetActionResult(ctx, req)
			require.NoError(t, err)
			require.Equal(t, text, string(r.StdoutRaw))
		}
	}
}

// The harness does not expose lifecycle controls. Locate its Docker socket by
// inspecting only the exact fixture-owned container, never a shared component.
func stopBackend(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	sockets, _ := filepath.Glob("/tmp/bbval-docker-*/docker.sock")
	hosts := []string{""}
	for _, s := range sockets {
		hosts = append(hosts, "unix://"+s)
	}
	for _, host := range hosts {
		args := []string{}
		if host != "" {
			args = append(args, "--host", host)
		}
		probe := append(append([]string{}, args...), "inspect", name)
		if exec.CommandContext(ctx, "docker", probe...).Run() != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, "docker", append(args, "stop", "--time", "1", name)...).CombinedOutput()
		require.NoError(t, err, string(out))
		return
	}
	t.Fatal("fixture Docker socket not found")
}

func TestBackendUnavailable(t *testing.T) {
	f, ctx := fixture(t)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	data := []byte("available offline after read through")
	d, err := app.Upload(ctx, instance, data)
	require.NoError(t, err)
	got, err := proxy.Read(ctx, instance, d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	got, err = proxy.Read(local(ctx), instance, d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	stopBackend(t, ctx, f.Endpoint(harness.App).Container)
	got, err = proxy.Read(ctx, instance, d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	short, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	got, err = proxy.Read(short, instance, harness.Digest([]byte("uncached offline")))
	require.Error(t, err)
	require.Empty(t, got)
	require.Contains(t, []codes.Code{codes.Unavailable, codes.DeadlineExceeded}, status.Code(err))
}
