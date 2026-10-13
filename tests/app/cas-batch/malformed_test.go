package casbatch

import (
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"testing"
)

func TestMalformedDigestIsPerItemFailure(t *testing.T) {
	ctx, c := client(t)
	good := []byte("malformed-peer")
	d, err := c.Upload(ctx, "malformed", good)
	require.NoError(t, err)
	bad := &repb.Digest{Hash: "not-a-sha256", SizeBytes: 1}
	t.Run("update", func(t *testing.T) {
		r, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "malformed", DigestFunction: repb.DigestFunction_SHA256, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: bad, Data: []byte("x")}, {Digest: d, Data: good}}})
		require.NoError(t, err, "invalid digest must not abort the valid peer")
		require.Len(t, r.Responses, 2)
		for _, item := range r.Responses {
			want := codes.OK
			if item.Digest.Hash == bad.Hash {
				want = codes.InvalidArgument
			}
			require.Equal(t, int32(want), item.GetStatus().GetCode())
		}
	})
	t.Run("read", func(t *testing.T) {
		r, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "malformed", DigestFunction: repb.DigestFunction_SHA256, Digests: []*repb.Digest{bad, d}})
		require.NoError(t, err, "invalid digest must not abort the valid peer")
		require.Len(t, r.Responses, 2)
		for _, item := range r.Responses {
			want := codes.OK
			if item.Digest.Hash == bad.Hash {
				want = codes.InvalidArgument
			} else {
				require.Equal(t, good, item.Data)
			}
			require.Equal(t, int32(want), item.GetStatus().GetCode())
		}
	})
}
