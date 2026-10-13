package capabilities_test

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
)

func TestAdvertisedCacheBehavior(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			ctx := bounded(t)
			c := f.Clients(component)
			caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{InstanceName: "capabilities"})
			require.NoError(t, err)
			t.Logf("capabilities: %s", caps)
			require.EqualValues(t, 2, caps.GetLowApiVersion().GetMajor())
			require.EqualValues(t, 2, caps.GetHighApiVersion().GetMajor())
			cc := caps.GetCacheCapabilities()
			require.NotNil(t, cc)
			require.NotEmpty(t, cc.GetDigestFunctions())
			// abc has published vectors for each advertised function, including BLAKE3.
			data := []byte("abc")
			for _, df := range cc.GetDigestFunctions() {
				t.Run(df.String(), func(t *testing.T) {
					var sum []byte
					switch df {
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
						sum, err = hex.DecodeString("6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85")
					default:
						t.Fatalf("add a test vector for advertised digest %s", df)
					}
					require.NoError(t, err)
					d := &repb.Digest{Hash: hex.EncodeToString(sum), SizeBytes: int64(len(data))}
					update, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "capabilities", DigestFunction: df, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: data}}})
					require.NoError(t, err)
					require.Len(t, update.GetResponses(), 1)
					require.Zero(t, update.Responses[0].GetStatus().GetCode())
					missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "capabilities", DigestFunction: df, BlobDigests: []*repb.Digest{d}})
					require.NoError(t, err)
					require.Empty(t, missing.GetMissingBlobDigests())
					read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "capabilities", DigestFunction: df, Digests: []*repb.Digest{d}})
					require.NoError(t, err)
					require.Len(t, read.GetResponses(), 1)
					require.Zero(t, read.Responses[0].GetStatus().GetCode())
					require.Equal(t, data, read.Responses[0].Data)
				})
			}
			for _, compressor := range cc.GetSupportedBatchUpdateCompressors() {
				t.Run("batch-"+compressor.String(), func(t *testing.T) {
					plain := bytes.Repeat([]byte("advertised compression "), 1000)
					payload := plain
					if compressor == repb.Compressor_ZSTD {
						payload, err = harness.Compress(plain)
						require.NoError(t, err)
					} else {
						require.Equal(t, repb.Compressor_IDENTITY, compressor)
					}
					d := harness.Digest(plain)
					res, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "compressors", DigestFunction: repb.DigestFunction_SHA256, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: payload, Compressor: compressor}}})
					require.NoError(t, err)
					require.Len(t, res.GetResponses(), 1)
					require.Zero(t, res.Responses[0].GetStatus().GetCode())
					got, err := c.Read(ctx, "compressors", d)
					require.NoError(t, err)
					require.Equal(t, plain, got)
				})
			}
			// Identity is mandatory even when omitted from the advertised list.
			for _, compressor := range append([]repb.Compressor_Value{repb.Compressor_IDENTITY}, cc.GetSupportedCompressors()...) {
				t.Run("stream-"+compressor.String(), func(t *testing.T) {
					require.Contains(t, []repb.Compressor_Value{repb.Compressor_IDENTITY, repb.Compressor_ZSTD}, compressor)
					plain := []byte(fmt.Sprintf("stream %s %s", component, compressor))
					d, err := c.Write(ctx, "compressors", plain, compressor == repb.Compressor_ZSTD)
					require.NoError(t, err)
					got, err := c.Read(ctx, "compressors", d)
					require.NoError(t, err)
					require.Equal(t, plain, got)
					stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName("compressors", d, compressor == repb.Compressor_ZSTD)})
					require.NoError(t, err)
					var wire []byte
					for {
						r, err := stream.Recv()
						if err == io.EOF {
							break
						}
						require.NoError(t, err)
						wire = append(wire, r.Data...)
					}
					if compressor == repb.Compressor_ZSTD {
						wire, err = harness.Decompress(wire)
						require.NoError(t, err)
					}
					require.Equal(t, plain, wire)
				})
			}
			// Exercise a batch comfortably below the implementation read limit.
			large := bytes.Repeat([]byte("L"), 1<<20)
			limit := cc.GetMaxBatchTotalSizeBytes()
			require.True(t, limit == 0 || limit >= int64(len(large)), "regression batch must fit the advertised limit")
			d, err := c.Upload(ctx, "batch-limit", large)
			require.NoError(t, err)
			read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "batch-limit", Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, read.GetResponses(), 1)
			require.Zero(t, read.Responses[0].GetStatus().GetCode())
			require.Equal(t, large, read.Responses[0].Data)
			require.True(t, cc.GetActionCacheUpdateCapabilities().GetUpdateEnabled())
			ad := harness.Digest([]byte("action-" + string(component)))
			want := &repb.ActionResult{ExitCode: 17}
			_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: "capabilities", ActionDigest: ad, ActionResult: want, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: "capabilities", ActionDigest: ad, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.EqualValues(t, 17, got.GetExitCode())
		})
	}
}

func TestDisabledFeatures(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"remote_execution": harness.Config{"enable_remote_exec": false}, "cache": harness.Config{"zstd_transcoding_enabled": false}}}})
	ctx := bounded(t)
	c := f.Clients(harness.App)
	caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.False(t, caps.GetExecutionCapabilities().GetExecEnabled())
	require.NotContains(t, caps.GetCacheCapabilities().GetSupportedCompressors(), repb.Compressor_ZSTD)
	require.NotContains(t, caps.GetCacheCapabilities().GetSupportedBatchUpdateCompressors(), repb.Compressor_ZSTD)
	plain := []byte("disabled zstd upload")
	compressed, err := harness.Compress(plain)
	require.NoError(t, err)
	update, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{DigestFunction: repb.DigestFunction_SHA256, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: harness.Digest(plain), Data: compressed, Compressor: repb.Compressor_ZSTD}}})
	require.NoError(t, err)
	require.Len(t, update.GetResponses(), 1)
	require.EqualValues(t, codes.Unimplemented, update.Responses[0].GetStatus().GetCode())
	d, err := c.Write(ctx, "disabled", []byte("identity remains supported"), false)
	require.NoError(t, err)
	_, err = c.Read(ctx, "disabled", d)
	require.NoError(t, err)
}

func TestAppExecutionAdvertisement(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	checkExecutionService(t, f.Clients(harness.App), true)
}
