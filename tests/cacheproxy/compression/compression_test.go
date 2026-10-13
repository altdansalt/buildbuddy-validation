package compression_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
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

func read(ctx context.Context, c *harness.Clients, d *repb.Digest, compressed bool, offset, limit int64) ([]byte, error) {
	s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName("compression", d, compressed), ReadOffset: offset, ReadLimit: limit})
	if err != nil {
		return nil, err
	}
	var b []byte
	for {
		r, e := s.Recv()
		if e == io.EOF {
			return b, nil
		}
		if e != nil {
			return b, e
		}
		b = append(b, r.Data...)
	}
}
func write(ctx context.Context, c *harness.Clients, d *repb.Digest, data []byte) (*bspb.WriteResponse, error) {
	s, e := c.ByteStream.Write(ctx)
	if e != nil {
		return nil, e
	}
	name := harness.UploadResourceName("compression", d, true)
	// Split inside the zstd frame to exercise compressed byte offset accounting.
	for start := 0; start < len(data); {
		end := start + 7
		if end > len(data) {
			end = len(data)
		}
		e = s.Send(&bspb.WriteRequest{ResourceName: name, WriteOffset: int64(start), Data: data[start:end], FinishWrite: end == len(data)})
		if e != nil && e != io.EOF {
			return nil, e
		}
		if e == io.EOF {
			break
		}
		start = end
	}
	return s.CloseAndRecv()
}
func fixture(t *testing.T) (context.Context, *harness.Clients, *harness.Clients) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"cache": harness.Config{"pebble": harness.Config{"min_bytes_auto_zstd_compression": int64(1 << 30)}}}, harness.CacheProxy: {"cache": harness.Config{"pebble": harness.Config{"min_bytes_auto_zstd_compression": int64(1 << 30)}}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx, f.Clients(harness.CacheProxy), f.Clients(harness.App)
}
func TestCrossFormat(t *testing.T) {
	ctx, proxy, app := fixture(t)
	caps, e := proxy.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, e)
	require.Contains(t, caps.CacheCapabilities.SupportedCompressors, repb.Compressor_ZSTD)
	for _, n := range []int{1, 1023, 4096, 1 << 20} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("size%d/compressed%v", n, compressed), func(t *testing.T) {
				data := bytes.Repeat([]byte{byte(n % 251), 42, 73}, n)
				data = append(data, []byte(fmt.Sprint(n, compressed))...)
				var d *repb.Digest
				if compressed {
					d = harness.Digest(data)
					z, e := harness.Compress(data)
					require.NoError(t, e)
					r, e := write(ctx, proxy, d, z)
					require.NoError(t, e)
					require.Equal(t, int64(len(z)), r.CommittedSize)
				} else {
					d, e = app.Write(ctx, "compression", data, false)
					require.NoError(t, e)
				}
				for pass := 0; pass < 2; pass++ {
					for _, format := range []bool{true, false} {
						b, e := read(ctx, proxy, d, format, 0, 0)
						require.NoError(t, e)
						if format {
							b, e = harness.Decompress(b)
							require.NoError(t, e)
						}
						require.Equal(t, data, b)
					}
				}
				b, e := app.Read(ctx, "compression", d)
				require.NoError(t, e)
				require.Equal(t, data, b)
				for _, offset := range []int64{1, int64(len(data) / 2), int64(len(data) - 1)} {
					b, e := read(ctx, proxy, d, true, offset, 0)
					require.NoError(t, e)
					b, e = harness.Decompress(b)
					require.NoError(t, e)
					require.Equal(t, data[offset:], b)
				}
				missing, e := proxy.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "compression", BlobDigests: []*repb.Digest{d}})
				require.NoError(t, e)
				require.Empty(t, missing.MissingBlobDigests)
			})
		}
	}
}
func TestInvalidCompressedUploads(t *testing.T) {
	ctx, proxy, app := fixture(t)
	for _, kind := range []string{"corrupt", "hash", "size"} {
		t.Run(kind, func(t *testing.T) {
			data := []byte("invalid upload " + kind)
			d := harness.Digest(data)
			z, e := harness.Compress(data)
			require.NoError(t, e)
			switch kind {
			case "corrupt":
				z = []byte("not a zstd frame")
			case "hash":
				z, e = harness.Compress([]byte("different content"))
				require.NoError(t, e)
			case "size":
				d.SizeBytes++
			}
			_, e = write(ctx, proxy, d, z)
			require.Error(t, e)
			t.Logf("%s upload status: %s", kind, status.Code(e))
			if kind != "corrupt" {
				require.Equal(t, codes.InvalidArgument, status.Code(e))
			}
			for _, c := range []*harness.Clients{proxy, app} {
				r, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "compression", BlobDigests: []*repb.Digest{d}})
				require.NoError(t, e)
				require.Len(t, r.MissingBlobDigests, 1)
			}
		})
	}
	// A malformed stream must not prevent subsequent valid operations.
	_, e := proxy.Write(ctx, "compression", []byte("healthy after rejected streams"), true)
	require.NoError(t, e)
}
func TestBatchCompression(t *testing.T) {
	ctx, proxy, app := fixture(t)
	caps, e := proxy.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, e)
	require.Contains(t, caps.CacheCapabilities.SupportedBatchUpdateCompressors, repb.Compressor_ZSTD)
	data := bytes.Repeat([]byte("batch compression payload"), 4096)
	d := harness.Digest(data)
	z, e := harness.Compress(data)
	require.NoError(t, e)
	r, e := proxy.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "compression", Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: z, Compressor: repb.Compressor_ZSTD}}})
	require.NoError(t, e)
	require.Len(t, r.Responses, 1)
	require.Zero(t, r.Responses[0].GetStatus().GetCode())
	for pass := 0; pass < 2; pass++ {
		for _, accept := range [][]repb.Compressor_Value{nil, {repb.Compressor_ZSTD}} {
			r, e := proxy.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "compression", Digests: []*repb.Digest{d}, AcceptableCompressors: accept})
			require.NoError(t, e)
			require.Len(t, r.Responses, 1)
			v := r.Responses[0]
			require.Zero(t, v.GetStatus().GetCode())
			require.Equal(t, d.Hash, v.Digest.Hash)
			require.Equal(t, d.SizeBytes, v.Digest.SizeBytes)
			b := v.Data
			if v.Compressor == repb.Compressor_ZSTD {
				require.NotEmpty(t, accept)
				b, e = harness.Decompress(b)
				require.NoError(t, e)
			} else {
				require.Equal(t, repb.Compressor_IDENTITY, v.Compressor)
			}
			require.Equal(t, data, b)
		}
	}
	b, e := app.Read(ctx, "compression", d)
	require.NoError(t, e)
	require.Equal(t, data, b)
	bad := harness.Digest([]byte("batch mismatched digest"))
	r, e = proxy.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "compression", Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: bad, Data: z, Compressor: repb.Compressor_ZSTD}}})
	require.NoError(t, e)
	require.Len(t, r.Responses, 1)
	require.Equal(t, int32(codes.InvalidArgument), r.Responses[0].GetStatus().GetCode())
}

// Competing formats may complete early when the other upload wins. The
// compressed response then reports -1, rather than an uncompressed byte count.
func TestConcurrentFormats(t *testing.T) {
	ctx, proxy, app := fixture(t)
	data := make([]byte, 256<<10)
	_, err := rand.Read(data)
	require.NoError(t, err)
	d := harness.Digest(data)
	z, err := harness.Compress(data)
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, e := proxy.Write(ctx, "compression", data, false); errs <- e }()
	go func() {
		defer wg.Done()
		r, e := write(ctx, proxy, d, z)
		if e == nil && r.CommittedSize != -1 && r.CommittedSize != int64(len(z)) {
			e = fmt.Errorf("compressed committed size %d, want -1 or %d", r.CommittedSize, len(z))
		}
		errs <- e
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	for _, c := range []*harness.Clients{proxy, app} {
		for _, compressed := range []bool{false, true} {
			b, e := read(ctx, c, d, compressed, 0, 0)
			require.NoError(t, e)
			if compressed {
				b, e = harness.Decompress(b)
				require.NoError(t, e)
			}
			require.Equal(t, data, b)
		}
	}
	// Repeat an already-complete compressed upload: REAPI mandates -1.
	r, e := write(ctx, proxy, d, z)
	require.NoError(t, e)
	require.Equal(t, int64(-1), r.CommittedSize)
}
