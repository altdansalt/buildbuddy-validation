package casbatch

import (
	"bytes"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestBatchConformance(t *testing.T) {
	ctx, c := client(t)
	good := []byte("batch-valid")
	d := harness.Digest(good)
	empty := harness.Digest(nil)
	badHash := harness.Digest([]byte("different"))
	badSize := harness.Digest([]byte("wrong-size"))
	badSize.SizeBytes++
	missing := harness.Digest([]byte("absent"))
	reqs := []*repb.BatchUpdateBlobsRequest_Request{
		{Digest: d, Data: good}, {Digest: d, Data: good}, {Digest: empty},
		{Digest: badHash, Data: []byte("mismatch")}, {Digest: badSize, Data: []byte("wrong-size")},
	}
	r, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "conformance", Requests: reqs})
	require.NoError(t, err)
	require.Len(t, r.Responses, len(reqs))
	counts := map[string]int{}
	for _, item := range r.Responses {
		counts[item.Digest.Hash]++
		want := codes.OK
		if item.Digest.Hash == badHash.Hash || item.Digest.Hash == badSize.Hash {
			want = codes.InvalidArgument
		}
		require.Equal(t, int32(want), item.GetStatus().GetCode(), "%v", item)
	}
	require.Equal(t, 2, counts[d.Hash])
	fm, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "conformance", BlobDigests: []*repb.Digest{d, d, empty, badHash, badSize, missing}})
	require.NoError(t, err)
	missingHashes := []string{}
	for _, digest := range fm.MissingBlobDigests {
		missingHashes = append(missingHashes, digest.Hash)
	}
	require.ElementsMatch(t, []string{badHash.Hash, badSize.Hash, missing.Hash}, missingHashes)
	read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "conformance", Digests: []*repb.Digest{d, d, empty, missing}})
	require.NoError(t, err)
	require.Len(t, read.Responses, 4)
	counts = map[string]int{}
	for _, item := range read.Responses {
		counts[item.Digest.Hash]++
		if item.Digest.Hash == missing.Hash {
			require.Equal(t, int32(codes.NotFound), item.GetStatus().GetCode())
			require.Empty(t, item.Data)
		} else {
			require.Zero(t, item.GetStatus().GetCode())
			if item.Digest.Hash == d.Hash {
				require.Equal(t, good, item.Data)
			} else {
				require.Empty(t, item.Data)
			}
		}
	}
	require.Equal(t, 2, counts[d.Hash])
	for _, call := range []string{"update", "read", "missing"} {
		t.Run("empty-request-"+call, func(t *testing.T) {
			switch call {
			case "update":
				r, e := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{})
				require.NoError(t, e)
				require.Empty(t, r.Responses)
			case "read":
				r, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{})
				require.NoError(t, e)
				require.Empty(t, r.Responses)
			case "missing":
				r, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{})
				require.NoError(t, e)
				require.Empty(t, r.MissingBlobDigests)
			}
		})
	}
	caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	limit := caps.GetCacheCapabilities().GetMaxBatchTotalSizeBytes()
	// Zero leaves limits to the protocol; v2.314.0 uses a 4,000,000-byte gRPC batch budget.
	if limit == 0 {
		limit = 4_000_000
	}
	require.Greater(t, limit, int64(0))
	oversized := harness.Digest([]byte("oversized"))
	oversized.SizeBytes = limit + 1
	_, err = c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{oversized}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestBatchSizeBoundaries(t *testing.T) {
	ctx, c := client(t)
	// Keep protobuf overhead below the release's 4,000,000-byte gRPC ceiling.
	data := bytes.Repeat([]byte("b"), 3_999_000)
	d, err := c.Upload(ctx, "boundary", data)
	require.NoError(t, err)
	read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "boundary", Digests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Len(t, read.Responses, 1)
	require.Zero(t, read.Responses[0].GetStatus().GetCode())
	require.Equal(t, data, read.Responses[0].Data)
	oversized := bytes.Repeat([]byte("c"), 4_194_305)
	od := harness.Digest(oversized)
	r, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "boundary", Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: od, Data: oversized}}})
	require.NoError(t, err)
	require.Len(t, r.Responses, 1)
	require.Zero(t, r.Responses[0].GetStatus().GetCode())
	// The release accepts this upload, but limits BatchReadBlobs to its 4 MB budget.
	_, err = c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "boundary", Digests: []*repb.Digest{od}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	fm, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "boundary", BlobDigests: []*repb.Digest{od}})
	require.NoError(t, err)
	require.Empty(t, fm.MissingBlobDigests)
	dataRead, err := c.Read(ctx, "boundary", od)
	require.NoError(t, err)
	require.Equal(t, oversized, dataRead)
}
