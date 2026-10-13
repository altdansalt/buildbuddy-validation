package chunkstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	encodingbinary "encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	"google.golang.org/protobuf/encoding/protojson"
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
func metadataField(t *testing.T, b []byte, wanted protowire.Number) int64 {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			require.Positive(t, k)
			b = b[k:]
			if n == wanted {
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
func metadataSize(t *testing.T, b []byte) int64 { return metadataField(t, b, 5) }
func checkMetadata(t *testing.T, b []byte, d *repb.Digest) {
	t.Helper()
	require.Equal(t, d.SizeBytes, metadataSize(t, b))
	require.Positive(t, metadataField(t, b, 2), "stored bytes")
	for _, field := range []protowire.Number{3, 4} {
		timestamp := metadataField(t, b, field)
		require.Positive(t, timestamp, "metadata timestamp %d", field)
		require.LessOrEqual(t, timestamp, time.Now().Add(time.Second).UnixMicro())
	}
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
	blob      []byte
	directory *repb.Directory
	chunks    []*repb.Digest
}

// Each chunk upload is a model transition too, before any manifest mutation.
func checkUploaded(t *testing.T, ctx context.Context, f *harness.Fixture, binary string, d *repb.Digest, data []byte, keys []string) {
	t.Helper()
	for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
		for _, key := range keys {
			c := harness.Dial(t, f.Endpoint(peer).GRPC, key)
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Empty(t, missing.MissingBlobDigests)
			batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, batch.Responses, 1)
			require.Zero(t, batch.Responses[0].GetStatus().GetCode())
			require.Equal(t, data, batch.Responses[0].Data)
			read, err := c.Read(ctx, "", d)
			require.NoError(t, err)
			require.Equal(t, data, read)
			request := metadataRequest(d, "")
			var response []byte
			require.NoError(t, c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", &request, &response, grpc.ForceCodec(wireCodec{})))
			checkMetadata(t, response, d)
			out := filepath.Join(t.TempDir(), "chunk")
			cmd := exec.CommandContext(ctx, binary, "download", "--target=grpc://"+f.Endpoint(peer).GRPC, "--api_key="+key, "--output_file="+out, "/"+harness.ResourceName("", d, false))
			cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
			logs, err := cmd.CombinedOutput()
			require.NoError(t, err, string(logs))
			read, err = os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, data, read)
			if peer == harness.App {
				var ui []byte
				require.NoError(t, c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", &request, &ui, grpc.ForceCodec(wireCodec{})))
				checkMetadata(t, ui, d)
				web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "cacheType": "CAS", "digestFunction": "SHA256"}})
				require.NoError(t, err)
				require.Equal(t, fmt.Sprint(d.SizeBytes), fmt.Sprint(web["digestSizeBytes"]))
				require.Equal(t, fmt.Sprint(metadataField(t, response, 2)), fmt.Sprint(web["storedSizeBytes"]))
				require.Equal(t, fmt.Sprint(metadataField(t, response, 4)), fmt.Sprint(web["lastModifyUsec"]))
			}
		}
	}
	for _, path := range []string{"download", "view"} {
		request, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/"+path+"?bytestream_url="+url.QueryEscape("bytestream://app:1985/"+harness.ResourceName("", d, false)), nil)
		require.NoError(t, err)
		request.Header.Set("x-buildbuddy-api-key", keys[1])
		response, err := f.Web.Client.Do(request)
		require.NoError(t, err)
		dataRead, err := io.ReadAll(response.Body)
		response.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, response.StatusCode)
		require.Equal(t, data, dataRead)
	}
}

func TestChunkState(t *testing.T) {
	binary := cli(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{
		harness.App:        {"cache": harness.Config{"avg_chunk_size_bytes": int64(512 << 10), "min_chunked_read_fallback_size_bytes": int64(2 << 20)}},
		harness.CacheProxy: {"cache": harness.Config{"avg_chunk_size_bytes": int64(512 << 10), "min_chunked_read_fallback_size_bytes": int64(2 << 20)}},
	}})
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "chunk-second-client", []string{"CACHE_WRITE"})
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
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
			if component == harness.CacheProxy {
				m.directory = &repb.Directory{}
				leaf := harness.Digest([]byte("directory file content"))
				_, err := c.Upload(ctx, instance, []byte("directory file content"))
				require.NoError(t, err)
				for i := 0; i < 32000; i++ {
					m.directory.Files = append(m.directory.Files, &repb.FileNode{Name: fmt.Sprintf("file-%06d", i), Digest: leaf})
				}
				m.blob, err = proto.Marshal(m.directory)
				require.NoError(t, err)
				require.Greater(t, len(m.blob), 2<<20)
				require.Less(t, len(m.blob), 4000000)
			}
			d := harness.Digest(m.blob)
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, missing.MissingBlobDigests, 1)
			if m.directory != nil {
				// Tree APIs require a physical root in v2.314.0; the isolated bug target
				// exercises the missing chunk-manifest fallback without this upload.
				_, err := c.Upload(ctx, instance, m.blob)
				require.NoError(t, err)
				checkUploaded(t, ctx, f, binary, d, m.blob, []string{f.APIKey, secondKey})
			}
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
				checkUploaded(t, ctx, f, binary, cd, m.blob[start:end], []string{f.APIKey, secondKey})
			}
			check := func(label string) {
				for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
					for _, key := range []string{f.APIKey, secondKey} {
						c := harness.Dial(t, f.Endpoint(peer).GRPC, key)
						component := peer
						t.Helper()
						split, err := c.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{InstanceName: instance, BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
						require.NoError(t, err, label)
						require.Len(t, split.ChunkDigests, len(m.chunks))
						require.Equal(t, repb.ChunkingFunction_FAST_CDC_2020, split.ChunkingFunction)
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
						cmd := exec.CommandContext(ctx, binary, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+key, "--output_file="+out, "/"+harness.ResourceName(instance, d, false))
						cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
						logs, err := cmd.CombinedOutput()
						require.NoError(t, err, string(logs))
						b, err = os.ReadFile(out)
						require.NoError(t, err)
						require.True(t, bytes.Equal(m.blob, b))
						if m.directory != nil {
							// CLI's typed JSON view uses the same stored Directory state.
							typed := exec.CommandContext(ctx, binary, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+key, "--type=Directory", "/"+harness.ResourceName(instance, d, false))
							typed.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
							jsonBytes, err := typed.Output()
							require.NoError(t, err)
							var cliDirectory repb.Directory
							require.NoError(t, protojson.Unmarshal(jsonBytes, &cliDirectory))
							require.True(t, proto.Equal(m.directory, &cliDirectory))
							tree, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{RootDigest: d, DigestFunction: repb.DigestFunction_SHA256})
							require.NoError(t, err)
							var dirs []*repb.Directory
							for {
								response, err := tree.Recv()
								if err == io.EOF {
									break
								}
								require.NoError(t, err)
								dirs = append(dirs, response.Directories...)
							}
							require.Len(t, dirs, 1)
							require.True(t, proto.Equal(m.directory, dirs[0]))
							if component == harness.App {
								db, err := proto.Marshal(d)
								require.NoError(t, err)
								request := field(nil, 3, db)
								request = protowire.AppendVarint(protowire.AppendTag(request, 6, protowire.VarintType), 1)
								var result []byte
								uiStream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree", grpc.ForceCodec(wireCodec{}))
								require.NoError(t, err)
								require.NoError(t, uiStream.SendMsg(&request))
								require.NoError(t, uiStream.CloseSend())
								require.NoError(t, uiStream.RecvMsg(&result))
								foundDirectory := false
								// GetTreeResponse field 2 is the serialized Directory.
								for len(result) > 0 {
									n, typ, k := protowire.ConsumeTag(result)
									require.Positive(t, k)
									result = result[k:]
									if n == 2 {
										foundDirectory = true
										raw, k := protowire.ConsumeBytes(result)
										require.Positive(t, k)
										var dir repb.Directory
										require.NoError(t, proto.Unmarshal(raw, &dir))
										require.True(t, proto.Equal(m.directory, &dir))
										result = result[k:]
										break
									}
									k = protowire.ConsumeFieldValue(n, typ, result)
									require.Positive(t, k)
									result = result[k:]
								}
								require.True(t, foundDirectory)
								var terminal []byte
								require.Equal(t, io.EOF, uiStream.RecvMsg(&terminal))
								// Browser RPC transport: gRPC-framed protobuf over HTTP, including terminal status.
								framed := make([]byte, 5)
								encodingbinary.BigEndian.PutUint32(framed[1:], uint32(len(request)))
								framed = append(framed, request...)
								httpRequest, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/GetTree", bytes.NewReader(framed))
								require.NoError(t, err)
								httpRequest.Header.Set("Content-Type", "application/proto+prefixed")
								httpRequest.Header.Set("x-buildbuddy-api-key", key)
								httpResponse, err := f.Web.Client.Do(httpRequest)
								require.NoError(t, err)
								payload, err := io.ReadAll(httpResponse.Body)
								httpResponse.Body.Close()
								require.NoError(t, err)
								require.Equal(t, 200, httpResponse.StatusCode)
								var browserDirs []*repb.Directory
								sawStatus := false
								for len(payload) > 0 {
									require.GreaterOrEqual(t, len(payload), 5)
									flags, length := payload[0], int(encodingbinary.BigEndian.Uint32(payload[1:5]))
									payload = payload[5:]
									require.GreaterOrEqual(t, len(payload), length)
									message := payload[:length]
									payload = payload[length:]
									if flags == 0x80 {
										require.Contains(t, string(message), "Grpc-Status: 0")
										sawStatus = true
										require.Empty(t, payload)
										continue
									}
									require.Zero(t, flags)
									for len(message) > 0 {
										n, typ, k := protowire.ConsumeTag(message)
										require.Positive(t, k)
										message = message[k:]
										if n == 2 {
											raw, k := protowire.ConsumeBytes(message)
											require.Positive(t, k)
											var dir repb.Directory
											require.NoError(t, proto.Unmarshal(raw, &dir))
											browserDirs = append(browserDirs, &dir)
											message = message[k:]
										} else {
											k := protowire.ConsumeFieldValue(n, typ, message)
											require.Positive(t, k)
											message = message[k:]
										}
									}
								}
								require.True(t, sawStatus)
								require.Len(t, browserDirs, 1)
								require.True(t, proto.Equal(m.directory, browserDirs[0]))
								web, err := f.Web.RPC("GetTreeDirectorySizes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "rootDigest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "digestFunction": "SHA256"})
								require.NoError(t, err)
								sizes := web["sizes"].([]any)
								require.Len(t, sizes, 1)
								size := sizes[0].(map[string]any)
								require.Equal(t, d.Hash, size["digest"])
								require.Equal(t, fmt.Sprint(len(m.directory.Files)), fmt.Sprint(size["childCount"]))
								require.Equal(t, fmt.Sprint(int64(len(m.directory.Files))*m.directory.Files[0].Digest.SizeBytes+d.SizeBytes), fmt.Sprint(size["totalSize"]))
								sizesRequest := field(nil, 2, db)
								sizesRequest = protowire.AppendVarint(protowire.AppendTag(sizesRequest, 4, protowire.VarintType), 1)
								var sizesResult []byte
								require.NoError(t, c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", &sizesRequest, &sizesResult, grpc.ForceCodec(wireCodec{})))
								sizesFound := 0
								for len(sizesResult) > 0 {
									n, typ, k := protowire.ConsumeTag(sizesResult)
									require.Positive(t, k)
									sizesResult = sizesResult[k:]
									if n == 2 {
										entry, k := protowire.ConsumeBytes(sizesResult)
										require.Positive(t, k)
										require.Equal(t, int64(len(m.directory.Files)), metadataField(t, entry, 3))
										require.Equal(t, int64(len(m.directory.Files))*m.directory.Files[0].Digest.SizeBytes+d.SizeBytes, metadataField(t, entry, 2))
										sizesFound++
										sizesResult = sizesResult[k:]
									} else {
										k := protowire.ConsumeFieldValue(n, typ, sizesResult)
										require.Positive(t, k)
										sizesResult = sizesResult[k:]
									}
								}
								require.Equal(t, 1, sizesFound)
							}
						}

						// Metadata describes physical CAS entries, not logical chunk manifests.
						// Check every constituent through the public Cache service and app UI APIs.
						for _, cd := range m.chunks {
							req := metadataRequest(cd, instance)
							var res []byte
							require.NoError(t, c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", &req, &res, grpc.ForceCodec(wireCodec{})))
							checkMetadata(t, res, cd)
							if component == harness.App {
								var ui []byte
								require.NoError(t, c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", &req, &ui, grpc.ForceCodec(wireCodec{})))
								checkMetadata(t, ui, cd)
								require.Equal(t, metadataField(t, res, 2), metadataField(t, ui, 2))
								require.Equal(t, metadataField(t, res, 4), metadataField(t, ui, 4))
								web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": cd.Hash, "sizeBytes": fmt.Sprint(cd.SizeBytes)}, "instanceName": instance, "cacheType": "CAS", "digestFunction": "SHA256"}})
								require.NoError(t, err)
								require.Equal(t, fmt.Sprint(cd.SizeBytes), fmt.Sprint(web["digestSizeBytes"]))
								require.Equal(t, fmt.Sprint(metadataField(t, res, 2)), fmt.Sprint(web["storedSizeBytes"]))
								require.Equal(t, fmt.Sprint(metadataField(t, res, 4)), fmt.Sprint(web["lastModifyUsec"]))
								require.NotEmpty(t, web["lastAccessUsec"])
							}
						}
						// Full-blob metadata is a physical-storage view: app chunk manifests
						// alone do not create a full CAS entry, while proxy reads may warm one.
						req := metadataRequest(d, instance)
						var full []byte
						mdErr := c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", &req, &full, grpc.ForceCodec(wireCodec{}))
						if component == harness.App && m.directory == nil {
							require.Equal(t, codes.NotFound, status.Code(mdErr), "app stores manifest/chunks, not full entry")
						} else {
							require.NoError(t, mdErr, "proxy full reads warm physical entry")
							checkMetadata(t, full, d)
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
				}
				for _, path := range []string{"download", "view"} {
					bsURL := "bytestream://app:1985/" + harness.ResourceName(instance, d, false)
					req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/"+path+"?bytestream_url="+url.QueryEscape(bsURL), nil)
					require.NoError(t, err)
					req.Header.Set("x-buildbuddy-api-key", f.APIKey)
					rsp, err := f.Web.Client.Do(req)
					require.NoError(t, err)
					b, err := io.ReadAll(rsp.Body)
					rsp.Body.Close()
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, rsp.StatusCode, string(b))
					require.Equal(t, m.blob, b)
				}
			}
			for repeat := 0; repeat < 5; repeat++ {
				writer := component
				if repeat%2 == 1 {
					if writer == harness.App {
						writer = harness.CacheProxy
					} else {
						writer = harness.App
					}
				}
				c := harness.Dial(t, f.Endpoint(writer).GRPC, []string{f.APIKey, secondKey}[repeat%2])
				if repeat == 2 || repeat == 4 {
					// Replace the accepted mapping with different valid boundaries
					// for exactly the same bytes, then compare all reads again.
					var next []*repb.Digest
					step := 512 << 10
					if repeat == 4 {
						step = 256 << 10
					}
					for start := 0; start < len(m.blob); start += step {
						cd, err := c.Upload(ctx, instance, m.blob[start:min(start+step, len(m.blob))])
						require.NoError(t, err)
						next = append(next, cd)
						checkUploaded(t, ctx, f, binary, cd, m.blob[start:min(start+step, len(m.blob))], []string{f.APIKey, secondKey})
						check(fmt.Sprintf("upload replacement chunk %d preserves prior mapping", start/step))
					}
					m.chunks = next
				}
				res, err := c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{InstanceName: instance, BlobDigest: d, ChunkDigests: m.chunks, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				require.True(t, proto.Equal(d, res.BlobDigest))

				started := time.Now()
				// Mapping writes are synchronous forwarding; poll both peers with a bound.
				require.Eventually(t, func() bool {
					for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
						split, err := f.Clients(peer).CAS.SplitBlob(ctx, &repb.SplitBlobRequest{BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
						if err != nil || len(split.ChunkDigests) != len(m.chunks) {
							return false
						}
						for i, chunk := range split.ChunkDigests {
							if !proto.Equal(chunk, m.chunks[i]) {
								return false
							}
						}
					}
					return true
				}, 10*time.Second, 25*time.Millisecond)
				t.Logf("splice %d via %s mapping convergence observed after %s", repeat, writer, time.Since(started))
				check(fmt.Sprintf("splice %d", repeat))
				time.Sleep(50 * time.Millisecond)
				check(fmt.Sprintf("splice %d stable", repeat))
			}
			// Every unsupported registration leaves all state intact, for both clients/peers.
			for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
				for _, key := range []string{f.APIKey, secondKey} {
					client := harness.Dial(t, f.Endpoint(peer).GRPC, key)
					stream, err := client.CAS.RegisterChunkMapping(ctx)
					require.NoError(t, err)
					err = stream.Send(&repb.RegisterChunkMappingRequest{InstanceName: instance, BlobDigest: d, ChunkDigests: m.chunks, DigestFunction: repb.DigestFunction_SHA256})
					if err != nil {
						require.Equal(t, io.EOF, err)
					}
					_, err = stream.CloseAndRecv()
					require.Equal(t, codes.Unimplemented, status.Code(err))
					check("after unsupported registration via " + string(peer))
				}
			}
		})
	}
}
