package capabilities_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestSmallSplicedBlobReadable(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	ctx := bounded(t)
	caps, e := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, e)
	if !caps.GetCacheCapabilities().GetSpliceBlobSupport() {
		t.Skip("splice not advertised")
	}
	left, e := c.Upload(ctx, "", []byte("abc"))
	require.NoError(t, e)
	right, e := c.Upload(ctx, "", []byte("def"))
	require.NoError(t, e)
	want := harness.Digest([]byte("abcdef"))
	_, e = c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{BlobDigest: want, ChunkDigests: []*repb.Digest{left, right}, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, e)
	mapping, e := c.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{BlobDigest: want, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, e)
	require.Len(t, mapping.GetChunkDigests(), 2)
	require.True(t, proto.Equal(left, mapping.ChunkDigests[0]))
	require.True(t, proto.Equal(right, mapping.ChunkDigests[1]))
	got, e := c.Read(ctx, "", want)
	t.Logf("spliced blob read: data=%q error=%v", got, e)
	require.NoError(t, e, "successful splice must make the blob readable")
	require.Equal(t, []byte("abcdef"), got)
}
