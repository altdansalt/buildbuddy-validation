package chunkstate

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

// A blob spliced from existing chunks is readable (ByteStream, SplitBlob) but
// FindMissingBlobs reports it missing when 2 MiB < size <= 4 MiB (default config).
func TestSpliceFindMissing(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)

	data := make([]byte, 2_720_000)
	for i := range data {
		data[i] = byte(i*7 + i>>8)
	}
	var chunks []*repb.Digest
	for start := 0; start < len(data); start += 680_000 {
		d, err := c.Upload(ctx, "", data[start:start+680_000])
		require.NoError(t, err)
		chunks = append(chunks, d)
	}
	d := harness.Digest(data)
	_, err := c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{BlobDigest: d, ChunkDigests: chunks, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)

	// Controls: the other surfaces agree the blob exists.
	split, err := c.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Len(t, split.ChunkDigests, len(chunks))
	got, err := c.Read(ctx, "", d)
	require.NoError(t, err)
	require.Equal(t, data, got)

	// Divergence: poll 10s to rule out asynchronous visibility.
	var missing []*repb.Digest
	for started := time.Now(); time.Since(started) < 10*time.Second; time.Sleep(200 * time.Millisecond) {
		r, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		if missing = r.MissingBlobDigests; len(missing) == 0 {
			return
		}
	}
	require.Empty(t, missing, "ByteStream and SplitBlob serve the spliced blob, so FindMissingBlobs must report it present")
}
