package digestfunctions_test

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"os"
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
	"google.golang.org/protobuf/proto"
)

// BLAKE3 vectors are from https://github.com/BLAKE3-team/BLAKE3 and the
// published empty/abc examples; fixed vectors avoid adding a hashing dependency.
func digest(fn repb.DigestFunction_Value, data []byte) *repb.Digest {
	var sum []byte
	switch fn {
	case repb.DigestFunction_SHA1:
		h := sha1.Sum(data)
		sum = h[:]
	case repb.DigestFunction_SHA256:
		h := sha256.Sum256(data)
		sum = h[:]
	case repb.DigestFunction_SHA384:
		h := sha512.Sum384(data)
		sum = h[:]
	case repb.DigestFunction_SHA512:
		h := sha512.Sum512(data)
		sum = h[:]
	case repb.DigestFunction_BLAKE3:
		hashes := map[string]string{"": "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262", "abc": "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"}
		h, ok := hashes[string(data)]
		if !ok {
			panic("missing BLAKE3 vector")
		}
		return &repb.Digest{Hash: h, SizeBytes: int64(len(data))}
	default:
		panic("unsupported test digest")
	}
	return &repb.Digest{Hash: hex.EncodeToString(sum), SizeBytes: int64(len(data))}
}

// REAPI omits the function path segment for SHA families, but requires it for BLAKE3.
func resource(instance string, fn repb.DigestFunction_Value, d *repb.Digest) string {
	segment := ""
	if fn == repb.DigestFunction_BLAKE3 {
		segment = "blake3/"
	}
	return fmt.Sprintf("%s/blobs/%s%s/%d", instance, segment, d.Hash, d.SizeBytes)
}
func read(t *testing.T, ctx context.Context, c *harness.Clients, name string, want []byte) {
	t.Helper()
	s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: name})
	require.NoError(t, err)
	var got []byte
	for {
		r, err := s.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		got = append(got, r.Data...)
	}
	require.Equal(t, string(want), string(got))
}
func batchRead(t *testing.T, ctx context.Context, c *harness.Clients, instance string, fn repb.DigestFunction_Value, d *repb.Digest, want []byte) {
	t.Helper()
	r, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, DigestFunction: fn, Digests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Len(t, r.Responses, 1)
	require.Equal(t, int32(codes.OK), r.Responses[0].GetStatus().GetCode(), "%v", r)
	require.True(t, proto.Equal(d, r.Responses[0].Digest))
	require.Equal(t, string(want), string(r.Responses[0].Data))
}
func upload(t *testing.T, ctx context.Context, c *harness.Clients, instance string, fn repb.DigestFunction_Value, d *repb.Digest, data []byte) {
	t.Helper()
	r, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: fn, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: data}}})
	require.NoError(t, err)
	require.Len(t, r.Responses, 1)
	require.Equal(t, int32(codes.OK), r.Responses[0].GetStatus().GetCode(), "%v", r)
}

func TestAdvertisedDigestFunctions(t *testing.T) {
	// Register before Start so this runs after the harness saves container logs.
	var f *harness.Fixture
	t.Cleanup(func() {
		if f == nil {
			return
		}
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			path := filepath.Join(f.OutputDir, f.Endpoint(component).Container+".logs.txt")
			logs, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotContains(t, string(logs), "panic:", "container %s", component)
			require.NotContains(t, string(logs), "fatal error:", "container %s", component)
		}
	})
	f = harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	for _, instance := range []string{"", "tenant/digest-functions"} {
		a, err := app.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{InstanceName: instance})
		require.NoError(t, err)
		p, err := proxy.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{InstanceName: instance})
		require.NoError(t, err)
		require.True(t, proto.Equal(a, p), "capabilities differ: app=%v proxy=%v", a, p)
		for _, fn := range []repb.DigestFunction_Value{repb.DigestFunction_SHA1, repb.DigestFunction_SHA256, repb.DigestFunction_SHA384, repb.DigestFunction_SHA512, repb.DigestFunction_BLAKE3} {
			require.Contains(t, p.GetCacheCapabilities().GetDigestFunctions(), fn)
		}
	}
	for _, fn := range []repb.DigestFunction_Value{repb.DigestFunction_SHA1, repb.DigestFunction_SHA256, repb.DigestFunction_SHA384, repb.DigestFunction_SHA512, repb.DigestFunction_BLAKE3} {
		t.Run(fn.String(), func(t *testing.T) {
			data := []byte("abc")
			d := digest(fn, data)
			// Backend-only upload forces a proxy miss followed by a local cache hit.
			cold := "digest/cold/" + strings.ToLower(fn.String())
			upload(t, ctx, app, cold, fn, d, data)
			for i := 0; i < 2; i++ {
				batchRead(t, ctx, proxy, cold, fn, d, data)
				read(t, ctx, proxy, resource(cold, fn, d), data)
			}
			// Proxy batch uploads must reach the backend using the requested function.
			instance := "digest/batch/" + strings.ToLower(fn.String())
			upload(t, ctx, proxy, instance, fn, d, data)
			batchRead(t, ctx, app, instance, fn, d, data)
			missing, err := proxy.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, DigestFunction: fn, BlobDigests: []*repb.Digest{d, digest(fn, []byte{})}})
			require.NoError(t, err)
			require.Empty(t, missing.MissingBlobDigests)
			batchRead(t, ctx, proxy, instance, fn, digest(fn, []byte{}), nil)
			read(t, ctx, proxy, resource(instance, fn, digest(fn, []byte{})), nil)
			// Fresh instance prevents an existing blob from bypassing stream validation.
			streamInstance := "digest/stream/" + strings.ToLower(fn.String())
			name := resource(streamInstance, fn, d)
			uploadName := strings.Replace(name, "/blobs/", "/uploads/00000000-0000-4000-8000-000000000001/blobs/", 1)
			s, err := proxy.ByteStream.Write(ctx)
			require.NoError(t, err)
			require.NoError(t, s.Send(&bspb.WriteRequest{ResourceName: uploadName, Data: data[:1]}))
			require.NoError(t, s.Send(&bspb.WriteRequest{WriteOffset: 1, Data: data[1:], FinishWrite: true}))
			r, err := s.CloseAndRecv()
			require.NoError(t, err)
			require.Equal(t, int64(len(data)), r.CommittedSize)
			read(t, ctx, app, name, data)
			read(t, ctx, proxy, name, data)
			// Exercise the compressed resource path, which also carries the digest
			// function. The committed size may be -1 or the compressed wire size.
			compressed, err := harness.Compress(data)
			require.NoError(t, err)
			compressedInstance := "digest/compressed/" + strings.ToLower(fn.String())
			compressedName := strings.Replace(resource(compressedInstance, fn, d), "/blobs/", "/compressed-blobs/zstd/", 1)
			compressedUpload := strings.Replace(compressedName, "/compressed-blobs/", "/uploads/00000000-0000-4000-8000-000000000002/compressed-blobs/", 1)
			cs, err := proxy.ByteStream.Write(ctx)
			require.NoError(t, err)
			require.NoError(t, cs.Send(&bspb.WriteRequest{ResourceName: compressedUpload, Data: compressed, FinishWrite: true}))
			cr, err := cs.CloseAndRecv()
			require.NoError(t, err)
			require.True(t, cr.CommittedSize == -1 || cr.CommittedSize == int64(len(compressed)), "committed size %d", cr.CommittedSize)
			read(t, ctx, app, resource(compressedInstance, fn, d), data)
			batchRead(t, ctx, proxy, compressedInstance, fn, d, data)

			// Independent concurrent cold reads must all return the right bytes.
			concurrentInstance := "digest/concurrent/" + strings.ToLower(fn.String())
			upload(t, ctx, app, concurrentInstance, fn, d, data)
			for i := 0; i < 4; i++ {
				t.Run(fmt.Sprintf("concurrent-%d", i), func(t *testing.T) {
					t.Parallel()
					readCtx, stop := context.WithTimeout(context.Background(), time.Minute)
					defer stop()
					batchRead(t, readCtx, proxy, concurrentInstance, fn, d, data)
					read(t, readCtx, proxy, resource(concurrentInstance, fn, d), data)
				})
			}

			// A wrong hash must produce an individual INVALID_ARGUMENT response.
			bad := &repb.Digest{Hash: strings.Repeat("0", len(d.Hash)), SizeBytes: 3}
			// ByteStream must reject invalid hashes too; use a new instance so
			// upload deduplication cannot legitimately short-circuit validation.
			invalidStream := strings.Replace(resource("digest/invalid-stream/"+strings.ToLower(fn.String()), fn, bad), "/blobs/", "/uploads/00000000-0000-4000-8000-000000000003/blobs/", 1)
			bs, err := proxy.ByteStream.Write(ctx)
			require.NoError(t, err)
			require.NoError(t, bs.Send(&bspb.WriteRequest{ResourceName: invalidStream, Data: data, FinishWrite: true}))
			_, err = bs.CloseAndRecv()
			require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
			for _, c := range []*harness.Clients{app, proxy} {
				r, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "digest/invalid", DigestFunction: fn, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: bad, Data: data}}})
				require.NoError(t, err)
				require.Len(t, r.Responses, 1)
				require.Equal(t, int32(codes.InvalidArgument), r.Responses[0].GetStatus().GetCode(), "%v", r)
			}
		})
	}
}
