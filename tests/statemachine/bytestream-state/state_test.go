package bytestreamstate

import (
	"bytes"
	"context"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// These small wire messages use the released proto field numbers. Keeping the
// codec local avoids adding BuildBuddy source or generated APIs to the harness.
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
func number(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}
func value(t *testing.T, b []byte, want protowire.Number) ([]byte, uint64) {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			require.Positive(t, k)
			if n == want {
				return v, 0
			}
			b = b[k:]
		} else if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			require.Positive(t, k)
			if n == want {
				return nil, v
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			require.Positive(t, k)
			b = b[k:]
		}
	}
	return nil, 0
}
func unary(ctx context.Context, c *harness.Clients, method string, req []byte) ([]byte, error) {
	var out []byte
	err := c.Conn.Invoke(ctx, method, &req, &out, grpc.ForceCodec(wireCodec{}))
	return out, err
}
func stream(ctx context.Context, c *harness.Clients, method string, req []byte) ([]byte, error) {
	s, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, method, grpc.ForceCodec(wireCodec{}))
	if err != nil {
		return nil, err
	}
	if err = s.SendMsg(&req); err != nil {
		return nil, err
	}
	if err = s.CloseSend(); err != nil {
		return nil, err
	}
	var out []byte
	for {
		var b []byte
		err = s.RecvMsg(&b)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		v, typ, k := protowire.ConsumeTag(b)
		if v != 1 || typ != protowire.BytesType || k < 0 {
			return nil, fmt.Errorf("unexpected GetFile response")
		}
		data, _ := protowire.ConsumeBytes(b[k:])
		out = append(out, data...)
	}
}

func TestPublication(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	clients := map[harness.Component]*harness.Clients{harness.App: f.Clients(harness.App), harness.CacheProxy: f.Clients(harness.CacheProxy)}
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "second-writer", []string{"CACHE_WRITE"})
	second := map[harness.Component]*harness.Clients{}
	for component := range clients {
		second[component] = harness.Dial(t, f.Endpoint(component).GRPC, secondKey)
	}
	binary, err := exec.LookPath("bb")
	require.NoError(t, err, "CLI is a required conformance surface")
	// Reference model maps immutable CAS keys to bytes only after Write succeeds.
	model := map[string][]byte{}
	for _, writer := range []harness.Component{harness.App, harness.CacheProxy} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/zstd=%v", writer, compressed), func(t *testing.T) {
				data := make([]byte, 180003)
				var x uint32 = 731
				for i := range data {
					x = x*1664525 + 1013904223
					data[i] = byte(x >> 24)
				}
				copy(data, fmt.Sprintf("writer=%s compressed=%v", writer, compressed))
				d := harness.Digest(data)
				instance := "publication/nested"
				key := harness.ResourceName(instance, d, false)
				for _, c := range clients {
					missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
					require.NoError(t, err)
					require.Len(t, missing.MissingBlobDigests, 1)
					_, err = c.Read(ctx, instance, d)
					require.Equal(t, codes.NotFound, status.Code(err))
				}
				for _, encoding := range []bool{compressed, compressed, !compressed} {
					payload := data
					if encoding {
						var err error
						payload, err = harness.Compress(data)
						require.NoError(t, err)
					}
					writeClient := clients[writer]
					if encoding != compressed {
						writeClient = second[writer]
					}
					s, err := writeClient.ByteStream.Write(ctx)
					require.NoError(t, err)
					name := harness.UploadResourceName(instance, d, encoding)
					for offset := 0; offset < len(payload); {
						end := min(offset+8191, len(payload))
						err = s.Send(&bspb.WriteRequest{ResourceName: name, WriteOffset: int64(offset), Data: payload[offset:end], FinishWrite: end == len(payload)})
						if err == io.EOF {
							break
						}
						require.NoError(t, err)
						offset = end
					}
					res, err := s.CloseAndRecv()
					require.NoError(t, err)
					writeCompleted := time.Now()
					if encoding {
						require.Contains(t, []int64{int64(len(payload)), -1}, res.CommittedSize)
					} else {
						require.Equal(t, d.SizeBytes, res.CommittedSize)
					}
					model[key] = append([]byte(nil), data...)
					keys := make([]string, 0, len(model))
					for k := range model {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, key := range keys {
						data := model[key]
						d := harness.Digest(data)
						for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
							c := second[component]
							start := time.Now()
							for {
								r, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
								require.NoError(t, e)
								if len(r.MissingBlobDigests) == 0 {
									break
								}
								require.Len(t, r.MissingBlobDigests, 1)
								require.True(t, proto.Equal(d, r.MissingBlobDigests[0]))
								require.Less(t, time.Since(start), 10*time.Second, "presence convergence bound")
								time.Sleep(50 * time.Millisecond)
							}
							t.Logf("presence convergence %s %s: observed %s after Write completion (probe %s)", component, d.Hash, time.Since(writeCompleted), time.Since(start))
							for stable := 0; stable < 3; stable++ {
								present, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
								require.NoError(t, e)
								require.Empty(t, present.MissingBlobDigests)
								time.Sleep(10 * time.Millisecond)
							}
							got, err := c.Read(ctx, instance, d)
							require.NoError(t, err)
							require.Equal(t, model[key], got)
							for _, z := range []bool{false, true} {
								bs, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, z)})
								require.NoError(t, err)
								var got []byte
								for {
									r, e := bs.Recv()
									if e == io.EOF {
										break
									}
									require.NoError(t, e)
									got = append(got, r.Data...)
								}
								if z {
									got, err = harness.Decompress(got)
									require.NoError(t, err)
								}
								require.Equal(t, model[key], got)
							}
							batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
							require.NoError(t, err)
							require.Len(t, batch.Responses, 1)
							r := batch.Responses[0]
							require.Zero(t, r.Status.Code)
							require.True(t, proto.Equal(d, r.Digest))
							require.Contains(t, []repb.Compressor_Value{repb.Compressor_IDENTITY, repb.Compressor_ZSTD}, r.Compressor)
							got = r.Data
							if r.Compressor == repb.Compressor_ZSTD {
								got, err = harness.Decompress(got)
								require.NoError(t, err)
							}
							require.Equal(t, model[key], got)
							digest, err := proto.Marshal(d)
							require.NoError(t, err)
							rn := number(field(field(nil, 1, digest), 2, []byte(instance)), 4, 2)
							metadataReq := field(nil, 2, rn)
							md, err := unary(ctx, c, "/cache.service.Cache/GetMetadata", metadataReq)
							require.NoError(t, err)
							_, size := value(t, md, 5)
							require.Equal(t, uint64(len(data)), size)
							checkExpanded(t, ctx, f, c, component, instance, data, binary, secondKey)
							if component == harness.App {
								md, err = unary(ctx, c, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", metadataReq)
								require.NoError(t, err)
								_, size = value(t, md, 5)
								require.Equal(t, uint64(len(data)), size)
								web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": "CAS"}})
								require.NoError(t, err)
								require.Equal(t, fmt.Sprint(d.SizeBytes), fmt.Sprint(web["digestSizeBytes"]))
								uri := "bytestream://app:1985/" + key
								got, err = stream(ctx, c, "/api.v1.ApiService/GetFile", field(nil, 1, []byte(uri)))
								require.NoError(t, err)
								require.Equal(t, model[key], got)
								compressedURI := "bytestream://app:1985/" + harness.ResourceName(instance, d, true)
								compressedFile, err := stream(ctx, c, "/api.v1.ApiService/GetFile", field(nil, 1, []byte(compressedURI)))
								require.NoError(t, err)
								compressedFile, err = harness.Decompress(compressedFile)
								require.NoError(t, err)
								require.Equal(t, model[key], compressedFile)
								rangeReq := number(number(field(nil, 1, []byte(uri)), 2, 17), 3, 1001)
								out, err := unary(ctx, c, "/api.v1.ApiService/GetFileRange", rangeReq)
								require.NoError(t, err)
								got, _ = value(t, out, 1)
								require.Equal(t, data[17:1001], got)
								for _, path := range []string{"/file/download", "/file/view"} {
									req, err := http.NewRequestWithContext(ctx, "GET", f.Endpoint(component).HTTP+path+"?bytestream_url="+url.QueryEscape(uri), nil)
									require.NoError(t, err)
									req.Header.Set("x-buildbuddy-api-key", f.APIKey)
									resp, err := http.DefaultClient.Do(req)
									require.NoError(t, err)
									got, err = io.ReadAll(resp.Body)
									resp.Body.Close()
									require.NoError(t, err)
									require.Equal(t, 200, resp.StatusCode, string(got))
									require.True(t, bytes.Equal(data, got))
								}
							}
						}
					}
				}
			})
		}
	}
	t.Run("CLI", func(t *testing.T) {
		binary, err := exec.LookPath("bb")
		if err != nil {
			t.Fatal(err)
		}
		for component := range clients {
			for key, data := range model {
				cmd := exec.CommandContext(ctx, binary, "download", key, "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey)
				out, err := cmd.Output()
				require.NoError(t, err)
				require.Equal(t, data, out)
			}
		}
	})
}

// Directory publication checks imported from the audit: directory publication and frontend compressor metadata.
func TestDirectoryPublication(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary, err := exec.LookPath("bb")
	require.NoError(t, err)
	for _, writer := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(writer), func(t *testing.T) {
			instance := "audit/" + string(writer)
			data := bytes.Repeat([]byte("directory audit contents "+string(writer)+"\n"), 2000)
			fileDigest, err := f.Clients(writer).Write(ctx, instance, data, true)
			require.NoError(t, err)
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				c := f.Clients(component)
				got, err := c.Read(ctx, instance, fileDigest)
				require.NoError(t, err)
				require.Equal(t, data, got)
				checkExpanded(t, ctx, f, c, component, instance, data, binary, f.APIKey)
			}
			directory := &repb.Directory{Files: []*repb.FileNode{{Name: "audit.txt", Digest: fileDigest}}}
			serialized, err := proto.Marshal(directory)
			require.NoError(t, err)
			root, err := f.Clients(writer).Write(ctx, instance, serialized, true)
			require.NoError(t, err)
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				c := f.Clients(component)

				for _, content := range [][]byte{data, serialized} {
					got, err := c.Read(ctx, instance, harness.Digest(content))
					require.NoError(t, err)
					require.Equal(t, content, got)
					checkExpanded(t, ctx, f, c, component, instance, content, binary, f.APIKey)
				}
				readback, err := c.Read(ctx, instance, fileDigest)
				require.NoError(t, err)
				require.True(t, bytes.Equal(data, readback))
				tree, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: root})
				require.NoError(t, err)
				var dirs []*repb.Directory
				for {
					r, err := tree.Recv()
					if err == io.EOF {
						break
					}
					require.NoError(t, err)
					dirs = append(dirs, r.Directories...)
				}
				require.Len(t, dirs, 1)
				require.True(t, proto.Equal(directory, dirs[0]))
				command := exec.CommandContext(ctx, binary, "download", harness.ResourceName(instance, root, false), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--type=Directory")
				output, err := command.Output()
				require.NoError(t, err)
				var interpreted repb.Directory
				// CLI emits protobuf JSON; use its schema-aware decoder below.
				require.NoError(t, protojson.Unmarshal(output, &interpreted))
				require.True(t, proto.Equal(directory, &interpreted))
				destination := t.TempDir()
				command = exec.CommandContext(ctx, binary, "download", harness.ResourceName(instance, root, false), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_directory="+destination)
				output, err = command.CombinedOutput()
				require.NoError(t, err, string(output))
				extracted, err := os.ReadFile(filepath.Join(destination, "audit.txt"))
				require.NoError(t, err)
				require.Equal(t, data, extracted)

				digestBytes, err := proto.Marshal(fileDigest)
				require.NoError(t, err)
				for _, compressor := range []uint64{0, 1} {
					rn := number(number(field(field(nil, 1, digestBytes), 2, []byte(instance)), 4, 2), 3, compressor)
					md, err := unary(ctx, c, "/cache.service.Cache/GetMetadata", field(nil, 2, rn))
					require.NoError(t, err)
					_, size := value(t, md, 5)
					require.Equal(t, uint64(len(data)), size)
					_, stored := value(t, md, 2)
					_, accessed := value(t, md, 3)
					_, modified := value(t, md, 4)
					require.Positive(t, stored)
					require.Positive(t, accessed)
					require.Positive(t, modified)
					t.Logf("writer=%s reader=%s compressor=%d digest=%d stored=%d access=%d modify=%d", writer, component, compressor, size, stored, accessed, modified)
					if component == harness.App {
						web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": fileDigest.Hash, "sizeBytes": fmt.Sprint(fileDigest.SizeBytes)}, "instanceName": instance, "cacheType": "CAS", "compressor": compressor}})
						require.NoError(t, err)
						require.Equal(t, fmt.Sprint(size), fmt.Sprint(web["digestSizeBytes"]))
						require.Equal(t, fmt.Sprint(stored), fmt.Sprint(web["storedSizeBytes"]))
						require.Equal(t, fmt.Sprint(modified), fmt.Sprint(web["lastModifyUsec"]))
					}
				}
				_, err = c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: harness.UploadResourceName(instance, fileDigest, true)})
				require.Equal(t, codes.Unimplemented, status.Code(err))
			}
			checkUITree(t, ctx, f, instance, root, directory)
			web, err := f.Web.RPC("GetTreeDirectorySizes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "rootDigest": map[string]any{"hash": root.Hash, "sizeBytes": fmt.Sprint(root.SizeBytes)}, "instanceName": instance})
			require.NoError(t, err)
			sizes, ok := web["sizes"].([]any)
			require.True(t, ok, "%v", web)
			require.Len(t, sizes, 1)
			row := sizes[0].(map[string]any)
			require.Equal(t, root.Hash, row["digest"])
			require.Equal(t, fmt.Sprint(root.SizeBytes+fileDigest.SizeBytes), fmt.Sprint(row["totalSize"]))
			require.Equal(t, "1", fmt.Sprint(row["childCount"]))
		})
	}
}
