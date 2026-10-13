package casbatch

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"testing"
)

// The REAPI requires a per-item INVALID_ARGUMENT when data does not match its digest.
func TestEmptyDigestRejectsNonemptyData(t *testing.T) {
	ctx, c := client(t)
	good := []byte("valid-peer")
	r, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{
		{Digest: harness.Digest(nil), Data: []byte("not empty")}, {Digest: harness.Digest(good), Data: good},
	}})
	require.NoError(t, err)
	require.Len(t, r.Responses, 2)
	for _, item := range r.Responses {
		want := codes.OK
		if item.Digest.Hash == harness.Digest(nil).Hash {
			want = codes.InvalidArgument
		}
		require.Equal(t, int32(want), item.GetStatus().GetCode(), "response: %v", item)
	}
}
