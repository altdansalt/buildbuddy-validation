package compression

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// A malformed compressed frame cannot represent the declared uncompressed digest.
func TestCorruptBatchZstdStatus(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	valid := []byte("valid sibling must survive a corrupt compressed batch entry")
	d := harness.Digest([]byte("invalid frame target"))
	rsp, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{
		{Digest: d, Data: []byte("not a zstd frame"), Compressor: repb.Compressor_ZSTD},
		{Digest: harness.Digest(valid), Data: valid},
	}})
	require.NoError(t, err)
	require.Len(t, rsp.Responses, 2)
	t.Logf("corrupt entry status: %v", rsp.Responses[0].Status)
	require.Zero(t, rsp.Responses[1].GetStatus().GetCode())
	got, err := c.Read(ctx, "", harness.Digest(valid))
	require.NoError(t, err)
	require.Equal(t, valid, got)
	missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Len(t, missing.MissingBlobDigests, 1)
	require.Equal(t, int32(codes.InvalidArgument), rsp.Responses[0].GetStatus().GetCode(), "malformed client data should not report an internal server failure")
}
