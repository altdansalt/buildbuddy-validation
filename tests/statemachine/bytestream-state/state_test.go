package bytestreamstate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
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
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	clients := map[harness.Component]*harness.Clients{harness.App: f.Clients(harness.App), harness.CacheProxy: f.Clients(harness.CacheProxy)}
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
					s, err := clients[writer].ByteStream.Write(ctx)
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
					if encoding {
						require.Contains(t, []int64{int64(len(payload)), -1}, res.CommittedSize)
					} else {
						require.Equal(t, d.SizeBytes, res.CommittedSize)
					}
					model[key] = append([]byte(nil), data...)
					for component, c := range clients {
						require.Eventually(t, func() bool {
							r, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
							return e == nil && len(r.MissingBlobDigests) == 0
						}, 10*time.Second, 50*time.Millisecond)
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
						if component == harness.App {
							md, err = unary(ctx, c, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", metadataReq)
							require.NoError(t, err)
							_, size = value(t, md, 5)
							require.Equal(t, uint64(len(data)), size)
							web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": "CAS"}})
							require.NoError(t, err)
							require.Equal(t, fmt.Sprint(d.SizeBytes), fmt.Sprint(web["digestSizeBytes"]))
							uri := "bytestream://" + f.Endpoint(component).GRPC + "/" + key
							got, err = stream(ctx, c, "/api.v1.ApiService/GetFile", field(nil, 1, []byte(uri)))
							require.NoError(t, err)
							require.Equal(t, model[key], got)
							compressedURI := "bytestream://" + f.Endpoint(component).GRPC + "/" + harness.ResourceName(instance, d, true)
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
			})
		}
	}
	t.Run("CLI", func(t *testing.T) {
		binary, err := exec.LookPath("bb")
		if err != nil {
			t.Skip("bb binary is not provided by the runner; see coverage notes")
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
