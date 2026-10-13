package casbatch

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"testing"
)

func TestMalformedZstdIsInvalidArgument(t *testing.T) {
	ctx, c := client(t)
	good := []byte("valid-compressed-peer")
	bad := harness.Digest([]byte("intended-uncompressed-data"))
	r, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "malformed-zstd", Requests: []*repb.BatchUpdateBlobsRequest_Request{
		{Digest: bad, Data: []byte("not a zstd frame"), Compressor: repb.Compressor_ZSTD},
		{Digest: harness.Digest(good), Data: good},
	}})
	require.NoError(t, err)
	require.Len(t, r.Responses, 2)
	// Check the valid peer first so a status failure cannot hide batch independence.
	for _, item := range r.Responses {
		if item.Digest.Hash == harness.Digest(good).Hash {
			require.Zero(t, item.GetStatus().GetCode())
		}
	}
	read, err := c.Read(ctx, "malformed-zstd", harness.Digest(good))
	require.NoError(t, err)
	require.Equal(t, good, read)
	for _, item := range r.Responses {
		if item.Digest.Hash == bad.Hash {
			require.Equal(t, int32(codes.InvalidArgument), item.GetStatus().GetCode(), "malformed client data: %v", item.GetStatus())
		}
	}
}
