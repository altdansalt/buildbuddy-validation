package casbatch

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Compare decoded contents: a server may choose identity even when zstd is accepted.
func readPayload(t *testing.T, r *repb.BatchReadBlobsResponse_Response, compressed bool) []byte {
	t.Helper()
	switch r.Compressor {
	case repb.Compressor_IDENTITY:
		return r.Data
	case repb.Compressor_ZSTD:
		require.True(t, compressed, "unrequested compression")
		data, err := harness.Decompress(r.Data)
		require.NoError(t, err)
		return data
	default:
		t.Fatalf("unexpected compressor %v", r.Compressor)
		return nil
	}
}

func digestKey(d *repb.Digest) string { return fmt.Sprintf("%s/%d", d.GetHash(), d.GetSizeBytes()) }

func TestRandomBatchReadIndependenceAndMissingAgreement(t *testing.T) {
	ctx, c := client(t)
	const seed int64 = 2026101303
	t.Logf("replay seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	limit := caps.GetCacheCapabilities().GetMaxBatchTotalSizeBytes()
	if limit == 0 {
		limit = 4_000_000
	}
	require.Greater(t, limit, int64(0))
	// Store one oversized blob via ByteStream, which is not subject to the batch budget.
	large := bytes.Repeat([]byte("round3-oversized"), int(limit/15)+1024)
	ld, err := c.Upload(ctx, "read-property", large)
	require.NoError(t, err)
	for round := 0; round < 24; round++ {
		t.Run(fmt.Sprintf("round-%02d", round), func(t *testing.T) {
			payloads := map[string][]byte{}
			present := map[string]bool{}
			var digests []*repb.Digest
			// Every round includes missing, identity-uploaded and zstd-uploaded items;
			// remaining kinds, sizes, contents, order and read negotiation are randomized.
			for i := 0; i < 12; i++ {
				kind := rng.Intn(3)
				if i < 3 {
					kind = i
				}
				data := make([]byte, 32+rng.Intn(8192))
				_, err := rng.Read(data)
				require.NoError(t, err)
				if kind == 2 {
					data = bytes.Repeat(data[:32], 1+rng.Intn(256))
				}
				d := harness.Digest(data)
				key := digestKey(d)
				require.NotContains(t, present, key)
				digests = append(digests, d)
				present[key] = kind != 0
				payloads[key] = data
				if kind != 0 {
					wire := data
					compressor := repb.Compressor_IDENTITY
					if kind == 2 {
						wire, err = harness.Compress(data)
						require.NoError(t, err)
						compressor = repb.Compressor_ZSTD
					}
					update, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "read-property", Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: wire, Compressor: compressor}}})
					require.NoError(t, err)
					require.Len(t, update.Responses, 1)
					require.Equal(t, key, digestKey(update.Responses[0].Digest))
					require.Zero(t, update.Responses[0].GetStatus().GetCode())
				}
			}
			rng.Shuffle(len(digests), func(i, j int) { digests[i], digests[j] = digests[j], digests[i] })
			// Test the same set with and without compression negotiation.
			for _, compressed := range []bool{false, true} {
				var compressors []repb.Compressor_Value
				if compressed {
					compressors = []repb.Compressor_Value{repb.Compressor_ZSTD}
				}
				singles := map[string]int32{}
				for _, d := range digests {
					single, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "read-property", Digests: []*repb.Digest{d}, AcceptableCompressors: compressors})
					require.NoError(t, err)
					require.Len(t, single.Responses, 1)
					r := single.Responses[0]
					key := digestKey(d)
					require.Equal(t, key, digestKey(r.Digest))
					want := int32(codes.NotFound)
					if present[key] {
						want = int32(codes.OK)
					}
					require.Equal(t, want, r.GetStatus().GetCode())
					singles[key] = want
					if present[key] {
						require.Equal(t, payloads[key], readPayload(t, r, compressed))
					} else {
						require.Empty(t, r.Data)
					}
				}
				mixed, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "read-property", Digests: digests, AcceptableCompressors: compressors})
				require.NoError(t, err)
				require.Len(t, mixed.Responses, len(digests))
				seen := map[string]bool{}
				missing := map[string]bool{}
				for _, r := range mixed.Responses {
					key := digestKey(r.Digest)
					require.Contains(t, singles, key)
					require.False(t, seen[key])
					seen[key] = true
					require.Equal(t, singles[key], r.GetStatus().GetCode())
					if present[key] {
						require.Equal(t, payloads[key], readPayload(t, r, compressed))
					} else {
						require.Empty(t, r.Data)
						missing[key] = true
					}
				}
				fm, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "read-property", BlobDigests: digests})
				require.NoError(t, err)
				actual := map[string]bool{}
				for _, d := range fm.MissingBlobDigests {
					key := digestKey(d)
					require.False(t, actual[key])
					actual[key] = true
				}
				require.Equal(t, missing, actual, "FindMissing must agree with every BatchRead status")
			}
			// A random oversized missing digest or the stored oversized blob makes the
			// whole BatchRead request invalid, independently of compression negotiation.
			oversized := ld
			exists := rng.Intn(2) == 0
			if !exists {
				oversized = harness.Digest([]byte(fmt.Sprintf("oversized-missing-%d", round)))
				oversized.SizeBytes = limit + 1 + int64(rng.Intn(8192))
			}
			all := append(append([]*repb.Digest{}, digests...), oversized)
			rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
			for _, compressors := range [][]repb.Compressor_Value{nil, {repb.Compressor_ZSTD}} {
				for _, set := range [][]*repb.Digest{{oversized}, all} {
					_, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "read-property", Digests: set, AcceptableCompressors: compressors})
					require.Equal(t, codes.InvalidArgument, status.Code(err), "oversized batch budget")
				}
			}
			fm, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "read-property", BlobDigests: all})
			require.NoError(t, err)
			expected := map[string]bool{}
			for key, exists := range present {
				if !exists {
					expected[key] = true
				}
			}
			if !exists {
				expected[digestKey(oversized)] = true
			}
			actual := map[string]bool{}
			for _, d := range fm.MissingBlobDigests {
				key := digestKey(d)
				require.False(t, actual[key])
				actual[key] = true
			}
			require.Equal(t, expected, actual)
			// Rejected oversized requests must not disturb the readable siblings.
			for _, d := range digests {
				if present[digestKey(d)] {
					data, err := c.Read(ctx, "read-property", d)
					require.NoError(t, err)
					require.Equal(t, payloads[digestKey(d)], data)
				}
			}
		})
	}
	data, err := c.Read(ctx, "read-property", ld)
	require.NoError(t, err)
	require.Equal(t, large, data)
}
