package randcas_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var seed = flag.Int64("randcas.seed", 314159, "Replay seed")
var iterations = flag.Int("randcas.iterations", 10, "Cases per advertised digest function (at least 8)")

func digest(data []byte, fn repb.DigestFunction_Value) *repb.Digest {
	var h []byte
	switch fn {
	case repb.DigestFunction_SHA1:
		s := sha1.Sum(data)
		h = s[:]
	case repb.DigestFunction_SHA256:
		s := sha256.Sum256(data)
		h = s[:]
	case repb.DigestFunction_SHA384:
		s := sha512.Sum384(data)
		h = s[:]
	case repb.DigestFunction_SHA512:
		s := sha512.Sum512(data)
		h = s[:]
	case repb.DigestFunction_BLAKE3:
		h = blakeSum(data)
	default:
		panic(fmt.Sprintf("unsupported oracle %s", fn))
	}
	return &repb.Digest{Hash: hex.EncodeToString(h), SizeBytes: int64(len(data))}
}
func name(instance string, d *repb.Digest, fn repb.DigestFunction_Value, compressed bool) string {
	prefix := "blobs/"
	if compressed {
		prefix = "compressed-blobs/zstd/"
	}
	if fn == repb.DigestFunction_BLAKE3 {
		prefix += "blake3/"
	}
	return fmt.Sprintf("%s/%s%s/%d", instance, prefix, d.Hash, d.SizeBytes)
}
func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed=%d: %v", *seed, err)
	}
}
func equal(t *testing.T, want, got []byte, api string) {
	t.Helper()
	if !bytes.Equal(want, got) {
		t.Fatalf("seed=%d %s bytes differ: want length=%d hash=%s got length=%d hash=%s", *seed, api, len(want), digest(want, repb.DigestFunction_SHA256).Hash, len(got), digest(got, repb.DigestFunction_SHA256).Hash)
	}
}
func supports(list []repb.Compressor_Value, x repb.Compressor_Value) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}
func read(ctx context.Context, c *harness.Clients, n string, offset, limit int64) ([]byte, error) {
	s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: n, ReadOffset: offset, ReadLimit: limit})
	if err != nil {
		return nil, err
	}
	var b []byte
	for {
		r, e := s.Recv()
		if e == io.EOF {
			return b, nil
		}
		if e != nil {
			return nil, e
		}
		b = append(b, r.Data...)
	}
}
func write(t *testing.T, ctx context.Context, c *harness.Clients, n string, data []byte, compressed bool, rng *rand.Rand) {
	t.Helper()
	wire := data
	if compressed {
		var e error
		wire, e = harness.Compress(data)
		check(t, e)
	}
	s, e := c.ByteStream.Write(ctx)
	check(t, e)
	for off := 0; ; {
		end := off + 1 + rng.Intn(256<<10)
		if end > len(wire) {
			end = len(wire)
		}
		e = s.Send(&bspb.WriteRequest{ResourceName: n, WriteOffset: int64(off), Data: wire[off:end], FinishWrite: end == len(wire)})
		if e == io.EOF {
			break
		}
		check(t, e)
		off = end
		if end == len(wire) {
			break
		}
	}
	res, e := s.CloseAndRecv()
	check(t, e)
	if (!compressed && res.CommittedSize != int64(len(data))) || (compressed && res.CommittedSize != -1 && res.CommittedSize != int64(len(wire))) {
		t.Fatalf("seed=%d committed size=%d wire=%d raw=%d", *seed, res.CommittedSize, len(wire), len(data))
	}
}
func TestRandomCASAgreement(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	if *iterations < 8 {
		t.Fatalf("seed=%d iterations must be >=8", *seed)
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	caps := make([]*repb.CacheCapabilities, 2)
	for i, c := range clients {
		v, e := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
		check(t, e)
		caps[i] = v.GetCacheCapabilities()
		if caps[i] == nil {
			t.Fatal("no cache capabilities")
		}
		t.Logf("seed=%d endpoint=%d capabilities=%v", *seed, i, caps[i])
	}
	for _, fn := range caps[0].DigestFunctions {
		t.Run(fn.String(), func(t *testing.T) {
			t.Logf("seed=%d function=%s", *seed, fn)
			found := false
			for _, v := range caps[1].DigestFunctions {
				if fn == v {
					found = true
				}
			}
			if !found {
				t.Fatalf("seed=%d proxy does not advertise app function %s", *seed, fn)
			}
			rng := rand.New(rand.NewSource(*seed + int64(fn)*100003))
			instance := "rand-cas/" + strings.ToLower(fn.String())
			sizes := []int{0, 1, 63, 64, 1024, rng.Intn(256<<10) + 1, 1 << 20, 20 << 20, 20 << 20}
			for writer := range clients {
				batchSet(t, ctx, clients, caps, fn, rng, instance, writer)
			}
			var stored []*repb.Digest
			for iteration := 0; iteration < *iterations; iteration++ {
				t.Run(fmt.Sprintf("case-%d", iteration), func(t *testing.T) {
					n := rng.Intn(512 << 10)
					if iteration < len(sizes) {
						n = sizes[iteration]
					}
					data := make([]byte, n)
					rng.Read(data)
					if iteration%2 == 0 {
						for j := range data {
							data[j] = byte(j % 7)
						}
					}
					d := digest(data, fn)
					writer := (iteration / 2) % 2
					compressed := iteration%3 != 0 && iteration != 8 && supports(caps[writer].SupportedCompressors, repb.Compressor_ZSTD)
					t.Logf("seed=%d n=%d hash=%s writer=%d compressed=%v", *seed, n, d.Hash, writer, compressed)
					// Fresh random digest probes distinguish real absence from successful empty reads.
					absent := make([]byte, 37)
					rng.Read(absent)
					missing := digest(absent, fn)
					for endpoint, c := range clients {
						m, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, DigestFunction: fn, BlobDigests: []*repb.Digest{missing}})
						check(t, e)
						if len(m.MissingBlobDigests) != 1 || m.MissingBlobDigests[0].Hash != missing.Hash {
							t.Fatalf("seed=%d endpoint=%d missing probe=%v", *seed, endpoint, m)
						}
						_, e = read(ctx, c, name(instance, missing, fn, false), 0, 0)
						if status.Code(e) != codes.NotFound {
							t.Fatalf("seed=%d missing digest status=%v, want NotFound", *seed, e)
						}
					}
					// Use duplicate batch entries where valid, and stream blobs exceeding the batch cap.
					batchLimit := caps[writer].MaxBatchTotalSizeBytes
					if batchLimit <= 0 {
						batchLimit = 4 << 20
					}
					if int64(n)*2 <= batchLimit && iteration%2 == 0 {
						wire := data
						codec := repb.Compressor_IDENTITY
						if compressed && supports(caps[writer].SupportedBatchUpdateCompressors, repb.Compressor_ZSTD) {
							var e error
							wire, e = harness.Compress(data)
							check(t, e)
							codec = repb.Compressor_ZSTD
						}
						req := &repb.BatchUpdateBlobsRequest_Request{Digest: d, Data: wire, Compressor: codec}
						res, e := clients[writer].CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: fn, Requests: []*repb.BatchUpdateBlobsRequest_Request{req, req}})
						check(t, e)
						if len(res.Responses) == 0 {
							t.Fatalf("seed=%d empty batch response", *seed)
						}
						for _, r := range res.Responses {
							if r.GetStatus().GetCode() != 0 || r.GetDigest().GetHash() != d.Hash {
								t.Fatalf("seed=%d batch update=%v", *seed, res)
							}
						}
					} else {
						write(t, ctx, clients[writer], instance+fmt.Sprintf("/uploads/%08x-0000-4000-8000-%012x/", uint32(*seed), iteration)+strings.TrimPrefix(name(instance, d, fn, compressed), instance+"/"), data, compressed, rng)
					}
					stored = append(stored, d)
					probe := append(append([]*repb.Digest(nil), stored...), d, missing)
					for endpoint, c := range clients {
						m, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, DigestFunction: fn, BlobDigests: probe})
						check(t, e)
						if len(m.MissingBlobDigests) != 1 || m.MissingBlobDigests[0].Hash != missing.Hash {
							t.Fatalf("seed=%d endpoint=%d inconsistent missing=%v", *seed, endpoint, m)
						}
						got, e := read(ctx, c, name(instance, d, fn, false), 0, 0)
						check(t, e)
						equal(t, data, got, "ByteStream full")
						offset := 0
						if n > 0 {
							offset = rng.Intn(n)
						}
						limit := 0
						if n-offset > 0 {
							limit = 1 + rng.Intn(n-offset)
						}
						got, e = read(ctx, c, name(instance, d, fn, false), int64(offset), int64(limit))
						check(t, e)
						end := n
						if limit > 0 {
							end = offset + limit
						}
						equal(t, data[offset:end], got, "ByteStream range")
						if supports(caps[endpoint].SupportedCompressors, repb.Compressor_ZSTD) {
							got, e = read(ctx, c, name(instance, d, fn, true), int64(offset), 0)
							check(t, e)
							got, e = harness.Decompress(got)
							check(t, e)
							equal(t, data[offset:], got, "ByteStream zstd suffix")
						}
						readLimit := caps[endpoint].MaxBatchTotalSizeBytes
						if readLimit <= 0 {
							readLimit = 4 << 20
						}
						if int64(n)*2 <= readLimit {
							res, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, DigestFunction: fn, Digests: []*repb.Digest{d, d, missing}, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
							check(t, e)
							seen, notfound := 0, 0
							for _, r := range res.Responses {
								if r.GetDigest().GetHash() == missing.Hash {
									if r.GetStatus().GetCode() != 5 {
										t.Fatalf("seed=%d missing batch status=%v", *seed, r)
									}
									notfound++
									continue
								}
								if r.GetDigest().GetHash() != d.Hash || r.GetStatus().GetCode() != 0 {
									t.Fatalf("seed=%d batch read=%v", *seed, r)
								}
								b := r.Data
								if r.Compressor == repb.Compressor_ZSTD {
									b, e = harness.Decompress(b)
									check(t, e)
								} else if r.Compressor != repb.Compressor_IDENTITY {
									t.Fatalf("seed=%d unsupported returned compressor %s", *seed, r.Compressor)
								}
								equal(t, data, b, "BatchRead")
								seen++
							}
							if seen == 0 || notfound != 1 {
								t.Fatalf("seed=%d missing batch response entries present=%d absent=%d", *seed, seen, notfound)
							}
						}
					}
				})
			}
		})
	}
}

// Mixed sets exercise response-to-digest association rather than response order.
func batchSet(t *testing.T, ctx context.Context, clients []*harness.Clients, caps []*repb.CacheCapabilities, fn repb.DigestFunction_Value, rng *rand.Rand, instance string, writer int) {
	t.Helper()
	model := map[string][]byte{}
	var digests []*repb.Digest
	var requests []*repb.BatchUpdateBlobsRequest_Request
	limit := int64(256 << 10)
	for _, cap := range caps {
		if cap.MaxBatchTotalSizeBytes > 0 && cap.MaxBatchTotalSizeBytes < limit {
			limit = cap.MaxBatchTotalSizeBytes
		}
	}
	// Reserve space for one duplicate and keep the entire request below the cap.
	maxBlob := int(limit / 8)
	if maxBlob < 1 {
		t.Fatalf("seed=%d unusable batch cap=%d", *seed, limit)
	}
	for i := 0; i < 6; i++ {
		n := rng.Intn(maxBlob)
		if i == 0 {
			n = 0
		}
		data := make([]byte, n)
		rng.Read(data)
		d := digest(data, fn)
		model[d.Hash] = data
		digests = append(digests, d)
		wire, codec := data, repb.Compressor_IDENTITY
		if i%2 == 1 && supports(caps[writer].SupportedBatchUpdateCompressors, repb.Compressor_ZSTD) {
			var e error
			wire, e = harness.Compress(data)
			check(t, e)
			codec = repb.Compressor_ZSTD
		}
		requests = append(requests, &repb.BatchUpdateBlobsRequest_Request{Digest: d, Data: wire, Compressor: codec})
	}
	requests = append(requests, requests[1])
	digests = append(digests, digests[1])
	rng.Shuffle(len(requests), func(i, j int) { requests[i], requests[j] = requests[j], requests[i] })
	rng.Shuffle(len(digests), func(i, j int) { digests[i], digests[j] = digests[j], digests[i] })
	res, e := clients[writer].CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: fn, Requests: requests})
	check(t, e)
	seen := map[string]bool{}
	for _, r := range res.Responses {
		_, known := model[r.GetDigest().GetHash()]
		if !known || r.GetStatus().GetCode() != 0 {
			t.Fatalf("seed=%d mixed batch update: %v", *seed, r)
		}
		seen[r.GetDigest().GetHash()] = true
	}
	if len(seen) != len(model) {
		t.Fatalf("seed=%d mixed update omitted digests", *seed)
	}
	for endpoint, c := range clients {
		missing, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, DigestFunction: fn, BlobDigests: digests})
		check(t, e)
		if len(missing.MissingBlobDigests) != 0 {
			t.Fatalf("seed=%d mixed set endpoint=%d missing=%v", *seed, endpoint, missing)
		}
		res, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, DigestFunction: fn, Digests: digests, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
		check(t, e)
		seen = map[string]bool{}
		for _, r := range res.Responses {
			want, known := model[r.GetDigest().GetHash()]
			if !known || r.GetStatus().GetCode() != 0 {
				t.Fatalf("seed=%d mixed batch read=%v", *seed, r)
			}
			got := r.Data
			if r.Compressor == repb.Compressor_ZSTD {
				got, e = harness.Decompress(got)
				check(t, e)
			} else if r.Compressor != repb.Compressor_IDENTITY {
				t.Fatalf("seed=%d unexpected compressor %s", *seed, r.Compressor)
			}
			equal(t, want, got, "mixed BatchRead")
			seen[r.GetDigest().GetHash()] = true
		}
		if len(seen) != len(model) {
			t.Fatalf("seed=%d mixed read omitted digests", *seed)
		}
		for _, d := range digests {
			got, e := read(ctx, c, name(instance, d, fn, false), 0, 0)
			check(t, e)
			equal(t, model[d.Hash], got, "mixed ByteStream")
		}
	}
}
