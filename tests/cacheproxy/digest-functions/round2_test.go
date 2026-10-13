package digestfunctions_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Oracle: Python blake3 1.0.11, official BLAKE3 implementation bindings.
//
//go:embed large_vectors.json
var largeVectors []byte

//go:embed random_vectors.json
var randomVectors []byte
var propertySeed = flag.Int64("digest_property_seed", 314159, "Replay seed for digest differential operation order")
var functions = []repb.DigestFunction_Value{repb.DigestFunction_SHA1, repb.DigestFunction_SHA256, repb.DigestFunction_SHA384, repb.DigestFunction_SHA512, repb.DigestFunction_BLAKE3}

type vector struct {
	Size int
	Hash string
}

func vectors(t *testing.T, raw []byte) []vector {
	t.Helper()
	var v []vector
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// Counter-mode SHA256 gives reproducible pseudorandom bytes independent of Go's PRNG.
func randomBlob(index, n int) []byte {
	b := make([]byte, 0, n+32)
	for counter := 0; len(b) < n; counter++ {
		var key [16]byte
		binary.LittleEndian.PutUint64(key[:8], uint64(index))
		binary.LittleEndian.PutUint64(key[8:], uint64(counter))
		h := sha256.Sum256(key[:])
		b = append(b, h[:]...)
	}
	return b[:n]
}
func vectorDigest(fn repb.DigestFunction_Value, b []byte, v vector) *repb.Digest {
	if fn == repb.DigestFunction_BLAKE3 {
		return &repb.Digest{Hash: v.Hash, SizeBytes: int64(v.Size)}
	}
	if fn == repb.DigestFunction_UNKNOWN {
		fn = repb.DigestFunction_SHA256
	}
	return digest(fn, b)
}
func streamUpload(t *testing.T, ctx context.Context, c *harness.Clients, name string, b []byte) {
	t.Helper()
	s, err := c.ByteStream.Write(ctx)
	require.NoError(t, err)
	name = strings.Replace(name, "/blobs/", "/uploads/00000000-0000-4000-8000-000000000042/blobs/", 1)
	for off := 0; off < len(b); {
		end := off + (1 << 20)
		if end > len(b) {
			end = len(b)
		}
		err = s.Send(&bspb.WriteRequest{ResourceName: name, WriteOffset: int64(off), Data: b[off:end], FinishWrite: end == len(b)})
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		off = end
	}
	r, err := s.CloseAndRecv()
	require.NoError(t, err)
	require.Equal(t, int64(len(b)), r.CommittedSize)
}
func streamCompare(t *testing.T, ctx context.Context, c *harness.Clients, name string, b []byte) {
	t.Helper()
	s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: name})
	require.NoError(t, err)
	off := 0
	for {
		r, err := s.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.LessOrEqual(t, off+len(r.Data), len(b))
		require.True(t, bytes.Equal(b[off:off+len(r.Data)], r.Data), "content mismatch at offset %d", off)
		off += len(r.Data)
	}
	require.Equal(t, len(b), off)
}
func TestLargeDigestInputs(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	for _, v := range vectors(t, largeVectors) {
		if v.Size < 16<<20 {
			continue
		}
		for _, fn := range functions {
			if v.Size > 16<<20 && fn != repb.DigestFunction_BLAKE3 {
				continue
			}
			t.Run(fmt.Sprintf("%s/%d", fn, v.Size), func(t *testing.T) {
				b := pattern(v.Size)
				d := vectorDigest(fn, b, v)
				instance := fmt.Sprintf("round2/large/%s/%d", fn, v.Size)
				name := resource(instance, fn, d)
				streamUpload(t, ctx, f.Clients(harness.CacheProxy), name, b)
				for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
					c := f.Clients(component)
					streamCompare(t, ctx, c, name, b)
					r, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, DigestFunction: fn, BlobDigests: []*repb.Digest{d}})
					require.NoError(t, err)
					require.Empty(t, r.MissingBlobDigests)
				}
				streamCompare(t, ctx, f.Clients(harness.CacheProxy), name, b)
			})
		}
	}
}
func TestSeededDigestAgreement(t *testing.T) {
	t.Logf("replay with -digest_property_seed=%d", *propertySeed)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	rng := rand.New(rand.NewSource(*propertySeed))
	vv := vectors(t, randomVectors)
	for _, i := range rng.Perm(len(vv)) {
		v := vv[i]
		b := randomBlob(i, v.Size)
		propertyFunctions := append(append([]repb.DigestFunction_Value{}, functions...), repb.DigestFunction_UNKNOWN)
		for _, j := range rng.Perm(len(propertyFunctions)) {
			fn := propertyFunctions[j]
			t.Run(fmt.Sprintf("blob%d/%s", i, fn), func(t *testing.T) {
				caseBlob := b
				if fn == repb.DigestFunction_UNKNOWN {
					caseBlob = randomBlob(i+len(vv), v.Size)
				}
				b := caseBlob
				d := vectorDigest(fn, b, v)
				instance := fmt.Sprintf("round2/property/%d/%d/%s", *propertySeed, i, fn)
				check := func(present bool) {
					var previous *repb.BatchReadBlobsResponse
					var missing *repb.FindMissingBlobsResponse
					for _, c := range []*harness.Clients{app, proxy} {
						m, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, DigestFunction: fn, BlobDigests: []*repb.Digest{d}})
						require.NoError(t, err)
						require.Equal(t, !present, len(m.MissingBlobDigests) == 1)
						if missing != nil {
							require.True(t, proto.Equal(missing, m))
						}
						missing = m
						r, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, DigestFunction: fn, Digests: []*repb.Digest{d}})
						require.NoError(t, err)
						require.Len(t, r.Responses, 1)
						want := codes.NotFound
						if present {
							want = codes.OK
							require.Equal(t, b, r.Responses[0].Data)
						}
						require.Equal(t, int32(want), r.Responses[0].GetStatus().GetCode())
						if previous != nil {
							require.Equal(t, previous.Responses[0].GetStatus().GetCode(), r.Responses[0].GetStatus().GetCode())
							require.Equal(t, previous.Responses[0].Data, r.Responses[0].Data)
						}
						previous = r
					}
					if present {
						streamCompare(t, ctx, app, resource(instance, fn, d), b)
						streamCompare(t, ctx, proxy, resource(instance, fn, d), b)
					}
				}
				check(false)
				writer := app
				if rng.Intn(2) == 0 {
					writer = proxy
				}
				upload(t, ctx, writer, instance, fn, d, b)
				check(true)
				check(true)
			})
		}
	}
}
func TestUnsupportedDigestEnums(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, fn := range []repb.DigestFunction_Value{repb.DigestFunction_MD5, repb.DigestFunction_VSO, repb.DigestFunction_MURMUR3, repb.DigestFunction_SHA256TREE, repb.DigestFunction_Value(-1), repb.DigestFunction_Value(123456)} {
		t.Run(fn.String(), func(t *testing.T) {
			data := []byte("enum probe")
			d := digest(repb.DigestFunction_SHA256, data)
			if fn == repb.DigestFunction_MD5 {
				h := md5.Sum(data)
				d.Hash = hex.EncodeToString(h[:])
			}
			var previous []codes.Code
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				t.Run(string(component), func(t *testing.T) {
					c := f.Clients(component)
					m, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "round2/enums", DigestFunction: fn, BlobDigests: []*repb.Digest{d}})
					mc := status.Code(err)
					if err == nil {
						require.Len(t, m.MissingBlobDigests, 1)
					}
					r, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "round2/enums", DigestFunction: fn, Digests: []*repb.Digest{d}})
					rc := status.Code(err)
					if err == nil {
						require.Len(t, r.Responses, 1)
						rc = codes.Code(r.Responses[0].GetStatus().GetCode())
						require.Empty(t, r.Responses[0].Data)
					}
					u, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "round2/enums", DigestFunction: fn, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: data}}})
					uc := status.Code(err)
					if err == nil {
						require.Len(t, u.Responses, 1)
						uc = codes.Code(u.Responses[0].GetStatus().GetCode())
					}
					t.Logf("%s: find=%s read=%s update=%s", component, mc, rc, uc)
					require.Contains(t, []codes.Code{codes.InvalidArgument, codes.Unimplemented}, uc, "unsupported writes must be rejected")
					require.Contains(t, []codes.Code{codes.InvalidArgument, codes.Unimplemented, codes.NotFound}, rc)
					got := []codes.Code{mc, rc, uc}
					if previous != nil {
						require.Equal(t, previous, got)
					}
					previous = got
				})
			}
		})
	}
}
