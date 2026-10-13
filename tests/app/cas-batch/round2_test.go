package casbatch

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestRandomBatchItemIndependence(t *testing.T) {
	ctx, c := client(t)
	const seed int64 = 2026101202
	t.Logf("replay seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	for round := 0; round < 24; round++ {
		t.Run(fmt.Sprintf("round-%02d", round), func(t *testing.T) {
			var items []*repb.BatchUpdateBlobsRequest_Request
			wants := map[string]int32{}
			payloads := map[string][]byte{}
			for i := 0; i < 12; i++ {
				data := make([]byte, 32+rng.Intn(4096))
				_, err := rng.Read(data)
				require.NoError(t, err)
				d := harness.Digest(data)
				wire := data
				compressor := repb.Compressor_IDENTITY
				kind := rng.Intn(4)
				if kind == 1 {
					d = harness.Digest(append(append([]byte{}, data...), 1))
				}
				if kind == 2 {
					d.SizeBytes++
				}
				if kind == 3 {
					wire, err = harness.Compress(data)
					require.NoError(t, err)
					compressor = repb.Compressor_ZSTD
				}
				item := &repb.BatchUpdateBlobsRequest_Request{Digest: d, Data: wire, Compressor: compressor}
				single, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "property", Requests: []*repb.BatchUpdateBlobsRequest_Request{item}})
				require.NoError(t, err)
				require.Len(t, single.Responses, 1)
				want := int32(codes.OK)
				if kind == 1 || kind == 2 {
					want = int32(codes.InvalidArgument)
				}
				require.Equal(t, want, single.Responses[0].GetStatus().GetCode())
				wants[d.Hash] = want
				payloads[d.Hash] = data
				items = append(items, item)
			}
			rng.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
			mixed, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "property", Requests: items})
			require.NoError(t, err)
			require.Len(t, mixed.Responses, len(items))
			seen := map[string]bool{}
			var digests []*repb.Digest
			for _, r := range mixed.Responses {
				want, ok := wants[r.GetDigest().GetHash()]
				require.True(t, ok)
				require.False(t, seen[r.Digest.Hash])
				seen[r.Digest.Hash] = true
				require.Equal(t, want, r.GetStatus().GetCode())
				digests = append(digests, r.Digest)
			}
			read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "property", Digests: digests})
			require.NoError(t, err)
			require.Len(t, read.Responses, len(items))
			seen = map[string]bool{}
			for _, r := range read.Responses {
				want, ok := wants[r.GetDigest().GetHash()]
				require.True(t, ok)
				require.False(t, seen[r.Digest.Hash])
				seen[r.Digest.Hash] = true
				if want == 0 {
					require.Zero(t, r.GetStatus().GetCode())
					require.Equal(t, payloads[r.Digest.Hash], r.Data)
				} else {
					require.Equal(t, int32(codes.NotFound), r.GetStatus().GetCode())
				}
			}
		})
	}
}

func TestConcurrentOverlappingCompressedBatches(t *testing.T) {
	ctx, c := client(t)
	var items []*repb.BatchUpdateBlobsRequest_Request
	payloads := map[string][]byte{}
	var digests []*repb.Digest
	for i := 0; i < 16; i++ {
		data := bytes.Repeat([]byte(fmt.Sprintf("overlap-%02d", i)), 1024)
		d := harness.Digest(data)
		items = append(items, &repb.BatchUpdateBlobsRequest_Request{Digest: d, Data: data})
		payloads[d.Hash] = data
		digests = append(digests, d)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	failures := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for iteration := 0; iteration < 10; iteration++ {
				batch := make([]*repb.BatchUpdateBlobsRequest_Request, 0, 8)
				uploaded := map[string]bool{}
				for j := 0; j < 8; j++ {
					base := items[(worker+j+iteration)%len(items)]
					wire := base.Data
					compressor := repb.Compressor_IDENTITY
					if (worker+iteration)%2 == 1 {
						var err error
						wire, err = harness.Compress(wire)
						if err != nil {
							failures <- err
							return
						}
						compressor = repb.Compressor_ZSTD
					}
					batch = append(batch, &repb.BatchUpdateBlobsRequest_Request{Digest: base.Digest, Data: wire, Compressor: compressor})
					uploaded[base.Digest.Hash] = true
				}
				update, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "overlap", Requests: batch})
				if err != nil {
					failures <- err
					return
				}
				if len(update.Responses) != len(batch) {
					failures <- fmt.Errorf("update cardinality: %d", len(update.Responses))
					return
				}
				for _, r := range update.Responses {
					if r.GetStatus().GetCode() != 0 {
						failures <- fmt.Errorf("update: %v", r)
						return
					}
				}
				read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "overlap", Digests: digests, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
				if err != nil {
					failures <- err
					return
				}
				if len(read.Responses) != len(digests) {
					failures <- fmt.Errorf("read cardinality")
					return
				}
				for _, r := range read.Responses {
					if r.GetStatus().GetCode() == int32(codes.NotFound) {
						if uploaded[r.Digest.Hash] {
							failures <- fmt.Errorf("acknowledged upload missing: %s", r.Digest.Hash)
							return
						}
						continue
					}
					if r.GetStatus().GetCode() != 0 {
						failures <- fmt.Errorf("read: %v", r)
						return
					}
					data := r.Data
					if r.Compressor == repb.Compressor_ZSTD {
						data, err = harness.Decompress(data)
					} else if r.Compressor != repb.Compressor_IDENTITY {
						err = fmt.Errorf("unexpected compressor %v", r.Compressor)
					}
					if err != nil || !bytes.Equal(data, payloads[r.Digest.Hash]) {
						failures <- fmt.Errorf("corrupt overlapping read %s: %v", r.Digest.Hash, err)
						return
					}
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "overlap", Digests: digests})
	require.NoError(t, err)
	require.Len(t, read.Responses, len(digests))
	for _, r := range read.Responses {
		require.Zero(t, r.GetStatus().GetCode())
		require.Equal(t, payloads[r.Digest.Hash], r.Data)
	}
}

func TestNonDefaultBatchDigestFunctions(t *testing.T) {
	ctx, c := client(t)
	caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	for _, fn := range []repb.DigestFunction_Value{repb.DigestFunction_SHA1, repb.DigestFunction_SHA384, repb.DigestFunction_SHA512} {
		t.Run(fn.String(), func(t *testing.T) {
			supported := false
			for _, advertised := range caps.GetCacheCapabilities().GetDigestFunctions() {
				if advertised == fn {
					supported = true
				}
			}
			if !supported {
				t.Skipf("%s not advertised", fn)
			}
			good := []byte("non-default-" + fn.String())
			var hash []byte
			switch fn {
			case repb.DigestFunction_SHA1:
				h := sha1.Sum(good)
				hash = h[:]
			case repb.DigestFunction_SHA384:
				h := sha512.Sum384(good)
				hash = h[:]
			case repb.DigestFunction_SHA512:
				h := sha512.Sum512(good)
				hash = h[:]
			}
			d := &repb.Digest{Hash: hex.EncodeToString(hash), SizeBytes: int64(len(good))}
			bad := &repb.Digest{Hash: d.Hash, SizeBytes: d.SizeBytes + 1}
			wire, err := harness.Compress(good)
			require.NoError(t, err)
			update, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "functions", DigestFunction: fn, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: wire, Compressor: repb.Compressor_ZSTD}, {Digest: bad, Data: good}}})
			require.NoError(t, err)
			require.Len(t, update.Responses, 2)
			for _, r := range update.Responses {
				want := int32(codes.OK)
				if r.Digest.SizeBytes == bad.SizeBytes {
					want = int32(codes.InvalidArgument)
				}
				require.Equal(t, want, r.GetStatus().GetCode())
			}
			read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "functions", DigestFunction: fn, Digests: []*repb.Digest{d}, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
			require.NoError(t, err)
			require.Len(t, read.Responses, 1)
			r := read.Responses[0]
			require.Zero(t, r.GetStatus().GetCode())
			data := r.Data
			if r.Compressor == repb.Compressor_ZSTD {
				data, err = harness.Decompress(data)
				require.NoError(t, err)
			} else {
				require.Equal(t, repb.Compressor_IDENTITY, r.Compressor)
			}
			require.Equal(t, good, data)
		})
	}
}

func TestBatchStoragePressure(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"cache": harness.Config{"max_size_bytes": int64(2 << 20)}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	rng := rand.New(rand.NewSource(2026101203))
	var digests []*repb.Digest
	payloads := map[string][]byte{}
	for round := 0; round < 12; round++ {
		var items []*repb.BatchUpdateBlobsRequest_Request
		for i := 0; i < 4; i++ {
			data := make([]byte, 256<<10)
			_, err := rng.Read(data)
			require.NoError(t, err)
			d := harness.Digest(data)
			digests = append(digests, d)
			payloads[d.Hash] = data
			items = append(items, &repb.BatchUpdateBlobsRequest_Request{Digest: d, Data: data})
		}
		update, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "pressure", Requests: items})
		require.NoError(t, err)
		require.Len(t, update.Responses, len(items))
		for _, r := range update.Responses {
			require.Zero(t, r.GetStatus().GetCode())
		}
	}
	for offset := 0; offset < len(digests); offset += 8 {
		read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "pressure", Digests: digests[offset : offset+8]})
		require.NoError(t, err)
		require.Len(t, read.Responses, 8)
		for _, r := range read.Responses {
			switch codes.Code(r.GetStatus().GetCode()) {
			case codes.OK:
				require.Equal(t, payloads[r.Digest.Hash], r.Data)
			case codes.NotFound:
				require.Empty(t, r.Data)
			default:
				t.Fatalf("unexpected storage-pressure response: %v", r)
			}
		}
	}
}

func TestUnsupportedBatchCompressorIsPerItem(t *testing.T) {
	ctx, c := client(t)
	good := []byte("compressor-valid-peer")
	unsupported := []byte("unsupported-compressor-payload")
	r, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "compressor-errors", Requests: []*repb.BatchUpdateBlobsRequest_Request{
		{Digest: harness.Digest(good), Data: good},
		{Digest: harness.Digest(unsupported), Data: unsupported, Compressor: repb.Compressor_Value(100)},
	}})
	require.NoError(t, err)
	require.Len(t, r.Responses, 2)
	for _, item := range r.Responses {
		if item.Digest.Hash == harness.Digest(good).Hash {
			require.Zero(t, item.GetStatus().GetCode())
		} else {
			require.Equal(t, int32(codes.Unimplemented), item.GetStatus().GetCode(), "%v", item)
		}
	}
}
