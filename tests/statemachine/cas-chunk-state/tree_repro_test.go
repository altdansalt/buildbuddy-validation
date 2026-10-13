package chunkstate

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// Successful chunk-only publication must be readable as a Directory by GetTree.
// Known release divergence; this target is tagged bug and excluded from normal CI.
func publishChunkOnly(t *testing.T, options harness.Options) (*harness.Fixture, context.Context, *repb.Digest, []byte, *repb.Directory, []*repb.Digest) {
	t.Helper()
	f := harness.Start(t, options)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	c := f.Clients(harness.CacheProxy)
	leaf, err := c.Upload(ctx, "", []byte("file content"))
	require.NoError(t, err)
	dir := &repb.Directory{}
	for i := 0; i < 32000; i++ {
		dir.Files = append(dir.Files, &repb.FileNode{Name: fmt.Sprintf("file-%06d", i), Digest: leaf})
	}
	data, err := proto.Marshal(dir)
	require.NoError(t, err)
	d := harness.Digest(data)
	var chunks []*repb.Digest
	for start := 0; start < len(data); start += 512 << 10 {
		chunk, err := c.Upload(ctx, "", data[start:min(start+(512<<10), len(data))])
		require.NoError(t, err)
		chunks = append(chunks, chunk)
	}
	_, err = c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{BlobDigest: d, ChunkDigests: chunks, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	return f, ctx, d, data, dir, chunks
}

func TestChunkOnlyGetTree(t *testing.T) {
	f, ctx, d, data, dir, chunks := publishChunkOnly(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"cache": harness.Config{"avg_chunk_size_bytes": int64(512 << 10)}}, harness.CacheProxy: {"cache": harness.Config{"avg_chunk_size_bytes": int64(512 << 10)}}}})
	app := f.Clients(harness.App)
	split, err := app.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Len(t, split.ChunkDigests, len(chunks))
	missing, err := app.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Empty(t, missing.MissingBlobDigests)
	batch, err := app.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Len(t, batch.Responses, 1)
	require.Zero(t, batch.Responses[0].GetStatus().GetCode())
	require.Equal(t, data, batch.Responses[0].Data)
	got, err := app.Read(ctx, "", d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	// No fault injection; a 512 KiB chunk average puts the root above the fallback threshold.
	started := time.Now()
	for time.Since(started) < 10*time.Second {
		stream, e := app.CAS.GetTree(ctx, &repb.GetTreeRequest{RootDigest: d, DigestFunction: repb.DigestFunction_SHA256})
		if e == nil {
			response, recvErr := stream.Recv()
			e = recvErr
			if e == nil {
				require.Len(t, response.Directories, 1)
				require.True(t, proto.Equal(dir, response.Directories[0]))
				return
			}
		}
		err = e
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("GetTree divergence remains after %s; SplitBlob, FindMissing, BatchRead and ByteStream succeed", time.Since(started))
	require.NoError(t, err, "GetTree must expose the successfully spliced root")
}

// Default-config FindMissingBlobs uses a different size gate than successful reads.
func TestChunkOnlyPresence(t *testing.T) {
	f, ctx, d, data, _, chunks := publishChunkOnly(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
		c := f.Clients(peer)
		split, err := c.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		require.Len(t, split.ChunkDigests, len(chunks))
		b, err := c.Read(ctx, "", d)
		require.NoError(t, err)
		require.Equal(t, data, b)
	}
	started := time.Now()
	var missing []*repb.Digest
	for time.Since(started) < 10*time.Second {
		// App is authoritative; proxy reads may warm only its own physical CAS.
		response, err := f.Clients(harness.App).CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		missing = response.MissingBlobDigests
		if len(missing) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("FindMissing divergence remains after %s with default config; SplitBlob and ByteStream succeed", time.Since(started))
	require.Empty(t, missing, "successful SpliceBlob must make the digest present")
}
