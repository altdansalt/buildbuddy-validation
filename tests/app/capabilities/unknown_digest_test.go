package capabilities_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestUnknownDigestFunctionRejected(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	ctx := bounded(t)
	caps, e := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, e)
	df := repb.DigestFunction_Value(999)
	require.NotContains(t, caps.GetCacheCapabilities().GetDigestFunctions(), df)
	// Control: valid SHA256 is supported and the blob is missing.
	d := harness.Digest([]byte("unknown digest control"))
	control, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{DigestFunction: repb.DigestFunction_SHA256, BlobDigests: []*repb.Digest{d}})
	require.NoError(t, e)
	require.Len(t, control.GetMissingBlobDigests(), 1)
	require.Equal(t, d.GetHash(), control.MissingBlobDigests[0].GetHash())
	require.Equal(t, d.GetSizeBytes(), control.MissingBlobDigests[0].GetSizeBytes())
	got, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{DigestFunction: df, BlobDigests: []*repb.Digest{d}})
	t.Logf("unknown digest response=%v error=%v", got, e)
	require.Contains(t, []codes.Code{codes.InvalidArgument, codes.Unimplemented}, status.Code(e), "unknown digest algorithm must be rejected")
}
