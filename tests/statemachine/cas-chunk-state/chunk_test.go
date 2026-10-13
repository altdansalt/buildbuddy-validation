package chunkstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// These messages use the released cache.proto wire schema, avoiding a dependency
// on BuildBuddy source/generated protos. The public harness connection supplies auth.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func field(b []byte, n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, n, protowire.BytesType), v)
}
func metadataRequest(d *repb.Digest, instance string) []byte {
	db, _ := proto.Marshal(d)
	rn := field(nil, 1, db)
	rn = field(rn, 2, []byte(instance))
	rn = protowire.AppendVarint(protowire.AppendTag(rn, 4, protowire.VarintType), 2) // CAS
	rn = protowire.AppendVarint(protowire.AppendTag(rn, 5, protowire.VarintType), 1) // SHA256
	return field(nil, 2, rn)
}
func metadataSize(t *testing.T, b []byte) int64 {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			require.Positive(t, k)
			b = b[k:]
			if n == 5 {
				return int64(v)
			}
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			require.Positive(t, k)
			b = b[k:]
		}
	}
	return 0
}
func cli(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	require.NoError(t, err)
	rsp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer rsp.Body.Close()
	require.Equal(t, 200, rsp.StatusCode)
	b, err := io.ReadAll(io.LimitReader(rsp.Body, 100<<20))
	require.NoError(t, err)
	require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(b)))
	p := filepath.Join(t.TempDir(), "bb")
	require.NoError(t, os.WriteFile(p, b, 0755))
	return p
}

type model struct {
	blob   []byte
	chunks []*repb.Digest
}

func TestChunkState(t *testing.T) {
	binary := cli(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{
		harness.App:        {"cache": harness.Config{"avg_chunk_size_bytes": int64(512 << 10), "min_chunked_read_fallback_size_bytes": int64(2 << 20)}},
		harness.CacheProxy: {"cache": harness.Config{"avg_chunk_size_bytes": int64(512 << 10), "min_chunked_read_fallback_size_bytes": int64(2 << 20)}},
	}})
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			c := f.Clients(component)
			instance := "" // bb download supports the default namespace in this release.
			caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{InstanceName: instance})
			require.NoError(t, err)
			require.True(t, caps.GetCacheCapabilities().GetSplitBlobSupport())
			require.True(t, caps.GetCacheCapabilities().GetSpliceBlobSupport())
			require.Equal(t, uint64(512<<10), caps.GetCacheCapabilities().GetFastCdc_2020Params().GetAvgChunkSizeBytes())
			m := model{blob: make([]byte, 3<<20)}
			// Deterministic, non-compressible content. Every endpoint has its own namespace.
			var x uint64 = 123456789
			if component == harness.CacheProxy {
				x = 987654321
			}
			for i := range m.blob {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				m.blob[i] = byte(x)
			}
			d := harness.Digest(m.blob)
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, missing.MissingBlobDigests, 1)
			// UNKNOWN is a valid unspecified chunking function; the unary implementation
			// accepts arbitrary verified ordered chunks. Keep every chunk below its max.
			for start := 0; start < len(m.blob); start += 1 << 20 {
				end := start + (1 << 20)
				if end > len(m.blob) {
					end = len(m.blob)
				}
				cd, err := c.Upload(ctx, instance, m.blob[start:end])
				require.NoError(t, err)
				m.chunks = append(m.chunks, cd)
			}
			check := func(label string) {
				t.Helper()
				split, err := c.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{InstanceName: instance, BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err, label)
				require.Len(t, split.ChunkDigests, len(m.chunks))
				var rebuilt []byte
				for i, cd := range split.ChunkDigests {
					require.True(t, proto.Equal(cd, m.chunks[i]))
					b, err := c.Read(ctx, instance, cd)
					require.NoError(t, err)
					rebuilt = append(rebuilt, b...)
				}
				require.Equal(t, m.blob, rebuilt)
				require.True(t, proto.Equal(d, harness.Digest(rebuilt)))
				missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				require.Empty(t, missing.MissingBlobDigests)
				batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				require.Len(t, batch.Responses, 1)
				require.Zero(t, batch.Responses[0].GetStatus().GetCode())
				require.Equal(t, m.blob, batch.Responses[0].Data)
				b, err := c.Read(ctx, instance, d)
				require.NoError(t, err)
				require.Equal(t, m.blob, b)
				out := filepath.Join(t.TempDir(), "blob")
				cmd := exec.CommandContext(ctx, binary, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+out, "/"+harness.ResourceName(instance, d, false))
				cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
				logs, err := cmd.CombinedOutput()
				require.NoError(t, err, string(logs))
				b, err = os.ReadFile(out)
				require.NoError(t, err)
				require.True(t, bytes.Equal(m.blob, b))
				// Metadata describes physical CAS entries, not logical chunk manifests.
				// Check every constituent through the public Cache service and app UI APIs.
				for _, cd := range m.chunks {
					req := metadataRequest(cd, instance)
					var res []byte
					require.NoError(t, c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", &req, &res, grpc.ForceCodec(wireCodec{})))
					require.Equal(t, cd.SizeBytes, metadataSize(t, res))
					if component == harness.App {
						var ui []byte
						require.NoError(t, c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", &req, &ui, grpc.ForceCodec(wireCodec{})))
						require.Equal(t, cd.SizeBytes, metadataSize(t, ui))
						web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": cd.Hash, "sizeBytes": fmt.Sprint(cd.SizeBytes)}, "instanceName": instance, "cacheType": "CAS", "digestFunction": "SHA256"}})
						require.NoError(t, err)
						require.Equal(t, fmt.Sprint(cd.SizeBytes), fmt.Sprint(web["digestSizeBytes"]))
					}
				}
				// Full-blob metadata is a physical-storage view: app chunk manifests
				// alone do not create a full CAS entry, while proxy reads may warm one.
				req := metadataRequest(d, instance)
				var full []byte
				mdErr := c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", &req, &full, grpc.ForceCodec(wireCodec{}))
				if mdErr == nil {
					require.Equal(t, d.SizeBytes, metadataSize(t, full))
				} else {
					require.Equal(t, codes.NotFound, status.Code(mdErr))
				}
				t.Logf("%s: logical blob present; physical metadata status=%s", label, status.Code(mdErr))
				if component == harness.App {
					var ui []byte
					uiErr := c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", &req, &ui, grpc.ForceCodec(wireCodec{}))
					require.Equal(t, status.Code(mdErr), status.Code(uiErr))
					web, webErr := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": "CAS", "digestFunction": "SHA256"}})
					if mdErr == nil {
						require.NoError(t, webErr)
						require.Equal(t, d.SizeBytes, metadataSize(t, ui))
						require.Equal(t, fmt.Sprint(d.SizeBytes), fmt.Sprint(web["digestSizeBytes"]))
					} else {
						require.Error(t, webErr)
						require.Contains(t, webErr.Error(), "NotFound")
					}
				}
				// Streaming mapping reads have no advertised support in this release.
				stream, err := c.CAS.GetChunkMapping(ctx, &repb.GetChunkMappingRequest{InstanceName: instance, BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
				if err == nil {
					_, err = stream.Recv()
				}
				require.Equal(t, codes.Unimplemented, status.Code(err))
			}
			for repeat := 0; repeat < 3; repeat++ {
				if repeat == 2 {
					// Replace the accepted mapping with different valid boundaries
					// for exactly the same bytes, then compare all reads again.
					var next []*repb.Digest
					for start := 0; start < len(m.blob); start += 512 << 10 {
						cd, err := c.Upload(ctx, instance, m.blob[start:start+(512<<10)])
						require.NoError(t, err)
						next = append(next, cd)
					}
					m.chunks = next
				}
				res, err := c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{InstanceName: instance, BlobDigest: d, ChunkDigests: m.chunks, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				require.True(t, proto.Equal(d, res.BlobDigest))
				check(fmt.Sprintf("splice %d", repeat))
			}
			// A valid streaming registration must report the explicit unsupported boundary.
			stream, err := c.CAS.RegisterChunkMapping(ctx)
			require.NoError(t, err)
			err = stream.Send(&repb.RegisterChunkMappingRequest{InstanceName: instance, BlobDigest: d, ChunkDigests: m.chunks, DigestFunction: repb.DigestFunction_SHA256})
			if err != nil {
				require.Equal(t, io.EOF, err)
			}
			_, err = stream.CloseAndRecv()
			require.Equal(t, codes.Unimplemented, status.Code(err))
			check("after unsupported registration")
		})
	}
}
