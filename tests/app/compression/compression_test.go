package compression

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMixedAPIRoundTrips(t *testing.T) {
	ctx, c := fixture(t)
	for _, mode := range []string{"batch-zstd", "stream-zstd", "batch-identity", "stream-identity"} {
		t.Run(mode, func(t *testing.T) {
			for _, size := range []int{0, 1, 4096, 65537} {
				t.Run(fmt.Sprint(size), func(t *testing.T) {
					data := bytes.Repeat([]byte(mode+"/payload/"), size)
					d := harness.Digest(data)
					z, err := harness.Compress(data)
					require.NoError(t, err)
					instance := "compression/nested/" + mode
					switch mode {
					case "batch-zstd", "batch-identity":
						payload, compressor := data, repb.Compressor_IDENTITY
						if mode == "batch-zstd" {
							payload, compressor = z, repb.Compressor_ZSTD
						}
						rsp, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: payload, Compressor: compressor}}})
						require.NoError(t, err)
						require.Len(t, rsp.Responses, 1)
						require.Zero(t, rsp.Responses[0].GetStatus().GetCode(), rsp.String())
					default:
						compressed := mode == "stream-zstd"
						payload := data
						if compressed {
							payload = z
						}
						rsp, err := write(ctx, c, harness.UploadResourceName(instance, d, compressed), payload)
						require.NoError(t, err)
						if compressed {
							require.True(t, rsp.CommittedSize == -1 || rsp.CommittedSize == int64(len(z)), "committed size %d, wire size %d", rsp.CommittedSize, len(z))
						} else {
							require.Equal(t, int64(len(data)), rsp.CommittedSize)
						}
					}
					got, err := c.Read(ctx, instance, d)
					require.NoError(t, err)
					require.Equal(t, string(data), string(got))
					got, err = read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, true)})
					require.NoError(t, err)
					plain, err := harness.Decompress(got)
					require.NoError(t, err)
					require.Equal(t, string(data), string(plain))
					for _, accepted := range [][]repb.Compressor_Value{nil, {repb.Compressor_ZSTD}} {
						rsp, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, AcceptableCompressors: accepted})
						require.NoError(t, err)
						require.Len(t, rsp.Responses, 1)
						r := rsp.Responses[0]
						require.Zero(t, r.GetStatus().GetCode())
						got = r.Data
						if r.Compressor == repb.Compressor_ZSTD {
							require.NotEmpty(t, accepted)
							got, err = harness.Decompress(got)
							require.NoError(t, err)
						} else {
							require.Equal(t, repb.Compressor_IDENTITY, r.Compressor)
						}
						require.Equal(t, string(data), string(got))
					}
				})
			}
		})
	}
}

func TestCompressedReadOffset(t *testing.T) {
	ctx, c := fixture(t)
	data := bytes.Repeat([]byte("offset payload/"), 4096)
	d, err := c.Upload(ctx, "offset", data)
	require.NoError(t, err)
	for _, off := range []int64{1, 1337, int64(len(data) - 1)} {
		b, err := read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName("offset", d, true), ReadOffset: off})
		require.NoError(t, err)
		got, err := harness.Decompress(b)
		require.NoError(t, err)
		require.Equal(t, data[off:], got)
	}
}

func TestCompressedDigestAndSizeValidation(t *testing.T) {
	ctx, c := fixture(t)
	data := []byte("valid compressed frame with invalid declared digest")
	z, err := harness.Compress(data)
	require.NoError(t, err)
	for _, kind := range []string{"hash", "size"} {
		d := harness.Digest(data)
		if kind == "hash" {
			d.Hash = harness.Digest([]byte("different")).Hash
		} else {
			d.SizeBytes++
		}
		rsp, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: z, Compressor: repb.Compressor_ZSTD}}})
		require.NoError(t, err)
		require.Equal(t, int32(codes.InvalidArgument), rsp.Responses[0].GetStatus().GetCode(), rsp.String())
		_, err = write(ctx, c, harness.UploadResourceName("", d, true), z)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, missing.MissingBlobDigests, 1)
	}
}

func TestConcurrentMixedWritesAndCorruptStream(t *testing.T) {
	ctx, c := fixture(t)
	data := bytes.Repeat([]byte("concurrent mixed compression payload/"), 8192)
	errors := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func(compressed bool) { _, err := c.Write(ctx, "concurrent", data, compressed); errors <- err }(i%2 == 0)
	}
	for i := 0; i < 4; i++ {
		require.NoError(t, <-errors)
	}
	got, err := c.Read(ctx, "concurrent", harness.Digest(data))
	require.NoError(t, err)
	require.Equal(t, string(data), string(got))
	for _, corrupt := range [][]byte{[]byte("not zstd"), {0x28, 0xb5, 0x2f, 0xfd}} {
		d := harness.Digest(append([]byte("corrupt target/"), corrupt...))
		_, err := write(ctx, c, harness.UploadResourceName("corrupt", d, true), corrupt)
		require.Error(t, err, "invalid or truncated frames must not be committed")
		t.Logf("corrupt ByteStream rejected: %v", err)
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "corrupt", BlobDigests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, missing.MissingBlobDigests, 1)
	}
}
