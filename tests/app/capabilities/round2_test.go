package capabilities_test

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"math/bits"
	"math/rand"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

var seed = flag.Int64("capabilities_seed", 31402, "replay seed for capabilities properties")

// Single-chunk BLAKE3 for the small protobufs used by the execution probe.
func blake3Small(data []byte) []byte {
	if len(data) > 1024 {
		panic("single-chunk BLAKE3 input too large")
	}
	iv := [8]uint32{0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19}
	cv := iv
	for block := 0; ; block++ {
		n := len(data)
		if n > 64 {
			n = 64
		}
		var buf [64]byte
		copy(buf[:], data[:n])
		var m [16]uint32
		for i := range m {
			m[i] = binary.LittleEndian.Uint32(buf[i*4:])
		}
		flags := uint32(0)
		if block == 0 {
			flags |= 1
		}
		if len(data) <= 64 {
			flags |= 2 | 8
		}
		v := [16]uint32{cv[0], cv[1], cv[2], cv[3], cv[4], cv[5], cv[6], cv[7], iv[0], iv[1], iv[2], iv[3], 0, 0, uint32(n), flags}
		g := func(a, b, c, d int, x, y uint32) {
			v[a] += v[b] + x
			v[d] = bits.RotateLeft32(v[d]^v[a], -16)
			v[c] += v[d]
			v[b] = bits.RotateLeft32(v[b]^v[c], -12)
			v[a] += v[b] + y
			v[d] = bits.RotateLeft32(v[d]^v[a], -8)
			v[c] += v[d]
			v[b] = bits.RotateLeft32(v[b]^v[c], -7)
		}
		for r := 0; r < 7; r++ {
			g(0, 4, 8, 12, m[0], m[1])
			g(1, 5, 9, 13, m[2], m[3])
			g(2, 6, 10, 14, m[4], m[5])
			g(3, 7, 11, 15, m[6], m[7])
			g(0, 5, 10, 15, m[8], m[9])
			g(1, 6, 11, 12, m[10], m[11])
			g(2, 7, 8, 13, m[12], m[13])
			g(3, 4, 9, 14, m[14], m[15])
			p := [16]int{2, 6, 3, 10, 7, 0, 4, 13, 1, 11, 12, 5, 9, 14, 15, 8}
			old := m
			for i := range m {
				m[i] = old[p[i]]
			}
		}
		for i := range cv {
			cv[i] = v[i] ^ v[i+8]
		}
		if len(data) <= 64 {
			out := make([]byte, 32)
			for i, x := range cv {
				binary.LittleEndian.PutUint32(out[i*4:], x)
			}
			return out
		}
		data = data[n:]
	}
}
func digestFor(t *testing.T, df repb.DigestFunction_Value, b []byte) *repb.Digest {
	t.Helper()
	var sum []byte
	switch df {
	case repb.DigestFunction_SHA1:
		h := sha1.Sum(b)
		sum = h[:]
	case repb.DigestFunction_SHA256:
		h := sha256.Sum256(b)
		sum = h[:]
	case repb.DigestFunction_SHA384:
		h := sha512.Sum384(b)
		sum = h[:]
	case repb.DigestFunction_SHA512:
		h := sha512.Sum512(b)
		sum = h[:]
	case repb.DigestFunction_BLAKE3:
		sum = blake3Small(b)
	default:
		t.Fatalf("unsupported test digest %s", df)
	}
	return &repb.Digest{Hash: hex.EncodeToString(sum), SizeBytes: int64(len(b))}
}
func TestDigestVectors(t *testing.T) {
	require.Equal(t, "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85", hex.EncodeToString(blake3Small([]byte("abc"))))
	require.Equal(t, "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262", hex.EncodeToString(blake3Small(nil)))
}

func TestExecutionDigestAndPriorityCapabilities(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	caps, err := c.Capabilities.GetCapabilities(bounded(t), &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	t.Logf("capabilities: %s", caps)
	ec := caps.GetExecutionCapabilities()
	require.True(t, ec.GetExecEnabled())
	dfs := ec.GetDigestFunctions()
	if len(dfs) == 0 {
		dfs = []repb.DigestFunction_Value{ec.GetDigestFunction()}
	}
	require.Contains(t, dfs, ec.GetDigestFunction())
	priorities := []int32{0}
	for _, r := range ec.GetExecutionPriorityCapabilities().GetPriorities() {
		require.LessOrEqual(t, r.MinPriority, r.MaxPriority)
		priorities = append(priorities, r.MinPriority, r.MaxPriority)
	}
	for _, df := range dfs {
		for _, priority := range priorities {
			t.Run(fmt.Sprintf("%s/%d", df, priority), func(t *testing.T) {
				ctx := bounded(t)
				instance := fmt.Sprintf("round2/%s/%d", df, priority)
				upload := func(m proto.Message) *repb.Digest {
					b, e := proto.Marshal(m)
					require.NoError(t, e)
					d := digestFor(t, df, b)
					r, e := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: df, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: b}}})
					require.NoError(t, e)
					require.Len(t, r.Responses, 1)
					require.Zero(t, r.Responses[0].GetStatus().GetCode())
					return d
				}
				root := upload(&repb.Directory{})
				cmd := upload(&repb.Command{Arguments: []string{"/bin/sh", "-c", "printf capabilities-round2 > out"}, OutputPaths: []string{"out"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
				action := upload(&repb.Action{CommandDigest: cmd, InputRootDigest: root, DoNotCache: true, Timeout: durationpb.New(20 * time.Second)})
				if df == repb.DigestFunction_SHA256 && priority == 0 {
					for _, r := range ec.GetExecutionPriorityCapabilities().GetPriorities() {
						for _, outside := range []int32{r.MinPriority - 1, r.MaxPriority + 1} {
							stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, DigestFunction: df, ExecutionPolicy: &repb.ExecutionPolicy{Priority: outside}})
							if err == nil {
								_, err = stream.Recv()
							}
							require.Equal(t, codes.InvalidArgument, status.Code(err), "out-of-range priority %d", outside)
						}
					}
				}
				s, e := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, DigestFunction: df, SkipCacheLookup: true, ExecutionPolicy: &repb.ExecutionPolicy{Priority: priority}})
				require.NoError(t, e)
				for {
					op, e := s.Recv()
					require.NoError(t, e)
					if !op.Done {
						continue
					}
					require.Nil(t, op.GetError())
					res := &repb.ExecuteResponse{}
					require.NoError(t, op.GetResponse().UnmarshalTo(res))
					require.Zero(t, res.GetStatus().GetCode(), "%v", res.GetStatus())
					require.Zero(t, res.GetResult().GetExitCode())
					require.Len(t, res.GetResult().GetOutputFiles(), 1)
					require.Equal(t, digestFor(t, df, []byte("capabilities-round2")), res.Result.OutputFiles[0].Digest)
					break
				}
			})
		}
	}
}

func TestSeededSplitSpliceCapabilities(t *testing.T) {
	t.Logf("replay seed=%d", *seed)
	rng := rand.New(rand.NewSource(*seed))
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	ctx := bounded(t)
	caps, e := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, e)
	cc := caps.GetCacheCapabilities()
	t.Logf("cache capabilities: %s", cc)
	for i := 0; i < 6; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			data := make([]byte, 1+rng.Intn(2<<20))
			_, e := rng.Read(data)
			require.NoError(t, e)
			instance := fmt.Sprintf("split-property/%d", i)
			d, e := c.Upload(ctx, instance, data)
			require.NoError(t, e)
			split, e := c.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{InstanceName: instance, BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
			if !cc.GetSplitBlobSupport() {
				require.Equal(t, codes.Unimplemented, status.Code(e))
				return
			}
			require.NoError(t, e)
			require.NotEmpty(t, split.GetChunkDigests())
			var joined []byte
			for _, chunk := range split.ChunkDigests {
				b, e := c.Read(ctx, instance, chunk)
				require.NoError(t, e)
				require.Equal(t, harness.Digest(b), chunk)
				joined = append(joined, b...)
			}
			require.Equal(t, data, joined)
			// Splice separately uploaded chunks into a blob that has never been uploaded.
			left := []byte(fmt.Sprintf("prefix-%d-", rng.Int63()))
			right := data
			ld, e := c.Upload(ctx, instance, left)
			require.NoError(t, e)
			rd := d
			want := append(bytes.Clone(left), right...)
			wd := harness.Digest(want)
			_, e = c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{InstanceName: instance, BlobDigest: wd, ChunkDigests: []*repb.Digest{ld, rd}, DigestFunction: repb.DigestFunction_SHA256})
			if !cc.GetSpliceBlobSupport() {
				require.Equal(t, codes.Unimplemented, status.Code(e))
				return
			}
			require.NoError(t, e)
			got, e := c.Read(ctx, instance, wd)
			require.NoError(t, e)
			require.Equal(t, want, got)
		})
	}
}

func TestUnadvertisedCacheChoicesRejected(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	ctx := bounded(t)
	caps, e := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, e)
	// UNKNOWN denotes inference, not an unsupported algorithm; identity compression
	// is mandatory even if omitted. Neither belongs in the negative matrix.
	for _, df := range []repb.DigestFunction_Value{repb.DigestFunction_MD5, repb.DigestFunction_MURMUR3, repb.DigestFunction_VSO, repb.DigestFunction_SHA256TREE, repb.DigestFunction_Value(999)} {
		if containsDigest(caps.GetCacheCapabilities().GetDigestFunctions(), df) {
			continue
		}
		t.Run(df.String(), func(t *testing.T) {
			_, e := c.CAS.FindMissingBlobs(bounded(t), &repb.FindMissingBlobsRequest{DigestFunction: df, BlobDigests: []*repb.Digest{{Hash: "900150983cd24fb0d6963f7d28e17f72", SizeBytes: 3}}})
			require.Contains(t, []codes.Code{codes.InvalidArgument, codes.Unimplemented}, status.Code(e), "unsupported digest must be rejected cleanly: %v", e)
		})
	}
	for _, comp := range []repb.Compressor_Value{repb.Compressor_DEFLATE, repb.Compressor_BROTLI, repb.Compressor_Value(999)} {
		t.Run(comp.String(), func(t *testing.T) {
			data := []byte("negative compressor")
			r, e := c.CAS.BatchUpdateBlobs(bounded(t), &repb.BatchUpdateBlobsRequest{DigestFunction: repb.DigestFunction_SHA256, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: harness.Digest(data), Data: data, Compressor: comp}}})
			if e != nil {
				require.Contains(t, []codes.Code{codes.InvalidArgument, codes.Unimplemented}, status.Code(e))
				return
			}
			require.Len(t, r.Responses, 1)
			require.Contains(t, []int32{int32(codes.InvalidArgument), int32(codes.Unimplemented)}, r.Responses[0].GetStatus().GetCode())
		})
	}
}
func containsDigest(dfs []repb.DigestFunction_Value, df repb.DigestFunction_Value) bool {
	for _, v := range dfs {
		if v == df {
			return true
		}
	}
	return false
}
