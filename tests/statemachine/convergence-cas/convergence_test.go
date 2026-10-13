package convergencecas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	binaryencoding "encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type blob struct {
	data    []byte
	digest  *repb.Digest
	present bool
}

// The public API's request URI and streamed response data occupy field 1,
// exactly like StringValue and BytesValue. These wire-compatible messages avoid
// adding BuildBuddy source/generated dependencies to this released-image test.
func getFile(ctx context.Context, c *harness.Clients, uri string) ([]byte, error) {
	s, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
	if err != nil {
		return nil, err
	}
	if err = s.SendMsg(wrapperspb.String(uri)); err != nil {
		return nil, err
	}
	if err = s.CloseSend(); err != nil {
		return nil, err
	}
	var data []byte
	for {
		m := new(wrapperspb.BytesValue)
		err := s.RecvMsg(m)
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
		data = append(data, m.Value...)
	}
}

// Minimal public wire fields: URI=1/start=2/end=3 and resource_name=2.
func getRange(ctx context.Context, c *harness.Clients, uri string, size int64) ([]byte, error) {
	req := wrapperspb.String(uri)
	wire := protowire.AppendTag(nil, 3, protowire.VarintType)
	wire = protowire.AppendVarint(wire, uint64(size))
	req.ProtoReflect().SetUnknown(wire)
	res := new(wrapperspb.BytesValue)
	err := c.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", req, res)
	return res.Value, err
}
func grpcMetadata(ctx context.Context, c *harness.Clients, d *repb.Digest) error {
	digest := protowire.AppendTag(nil, 1, protowire.BytesType)
	digest = protowire.AppendString(digest, d.Hash)
	digest = protowire.AppendTag(digest, 2, protowire.VarintType)
	digest = protowire.AppendVarint(digest, uint64(d.SizeBytes))
	resource := protowire.AppendTag(nil, 1, protowire.BytesType)
	resource = protowire.AppendBytes(resource, digest)
	resource = protowire.AppendTag(resource, 4, protowire.VarintType)
	resource = protowire.AppendVarint(resource, 2)
	req := new(wrapperspb.BytesValue)
	wire := protowire.AppendTag(nil, 2, protowire.BytesType)
	wire = protowire.AppendBytes(wire, resource)
	req.ProtoReflect().SetUnknown(wire)
	return c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", req, new(wrapperspb.BytesValue))
}

func httpRead(ctx context.Context, f *harness.Fixture, path string, body any) ([]byte, int, error) {
	var r io.Reader
	method := "GET"
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, f.Endpoint(harness.App).HTTP+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return b, res.StatusCode, err
}

func cli(t *testing.T, ctx context.Context) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	b, err := io.ReadAll(io.LimitReader(res.Body, 100<<20))
	require.NoError(t, err)
	require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(b)))
	p := filepath.Join(t.TempDir(), "bb")
	require.NoError(t, os.WriteFile(p, b, 0755))
	return p
}

func TestBatchCASConvergence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binary := cli(t, ctx)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "second-client", []string{"CACHE_WRITE"})
	clients := map[harness.Component]*harness.Clients{harness.App: f.Clients(harness.App), harness.CacheProxy: harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, secondKey)}
	// Empty CAS is canonical and logically present without any write.
	model := []*blob{{data: []byte{}, digest: harness.Digest(nil), present: true}}
	for _, data := range [][]byte{[]byte("app batch first\x00\xff"), bytes.Repeat([]byte("proxy flat blob\x00"), 8193), []byte("later app addition"), []byte("later proxy addition")} {
		model = append(model, &blob{data: data, digest: harness.Digest(data)})
	}
	leaf := &repb.Directory{Files: []*repb.FileNode{{Name: "payload", Digest: model[1].digest}}}
	leafData, leafDigest, err := harness.MarshalDigest(leaf)
	require.NoError(t, err)
	root := &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "child", Digest: leafDigest}}}
	rootData, rootDigest, err := harness.MarshalDigest(root)
	require.NoError(t, err)
	model = append(model, &blob{data: leafData, digest: leafDigest}, &blob{data: rootData, digest: rootDigest})
	check := func(ctx context.Context) error {
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			c := clients[component]
			var ds []*repb.Digest
			for _, b := range model {
				ds = append(ds, b.digest)
			}
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: ds, DigestFunction: repb.DigestFunction_SHA256})
			if err != nil {
				return err
			}
			absent := map[string]bool{}
			for _, d := range missing.MissingBlobDigests {
				if absent[d.Hash] {
					return fmt.Errorf("duplicate missing digest")
				}
				known := false
				for _, b := range model {
					if proto.Equal(b.digest, d) {
						known = true
					}
				}
				if !known {
					return fmt.Errorf("unsolicited missing digest %v", d)
				}
				absent[d.Hash] = true
			}
			reads, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: ds, DigestFunction: repb.DigestFunction_SHA256})
			if err != nil {
				return err
			}
			got := map[string]*repb.BatchReadBlobsResponse_Response{}
			for _, r := range reads.Responses {
				if got[r.GetDigest().GetHash()] != nil {
					return fmt.Errorf("duplicate batch response")
				}
				got[r.GetDigest().GetHash()] = r
			}
			if len(got) != len(model) {
				return fmt.Errorf("%s batch response count %d", component, len(got))
			}
			for _, b := range model {
				r := got[b.digest.Hash]
				if r == nil || !proto.Equal(r.Digest, b.digest) || absent[b.digest.Hash] == b.present {
					return fmt.Errorf("%s presence mismatch %s", component, b.digest.Hash)
				}
				data, err := c.Read(ctx, "", b.digest)
				if !b.present {
					mdErr := c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", metadataRequest(b.digest), new(wrapperspb.BytesValue))
					if status.Code(mdErr) != codes.NotFound {
						return fmt.Errorf("%s absent local metadata: %v", component, mdErr)
					}
					if r.GetStatus().GetCode() != int32(codes.NotFound) || status.Code(err) != codes.NotFound {
						return fmt.Errorf("%s absent blob read: batch=%v stream=%v", component, r, err)
					}
					continue
				}
				compressed, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{b.digest}, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}, DigestFunction: repb.DigestFunction_SHA256})
				if err != nil {
					return err
				}
				if len(compressed.Responses) != 1 {
					return fmt.Errorf("compressed response count")
				}
				cr := compressed.Responses[0]
				cd := cr.Data
				if cr.Compressor == repb.Compressor_ZSTD {
					cd, err = harness.Decompress(cd)
				} else if cr.Compressor != repb.Compressor_IDENTITY {
					return fmt.Errorf("unknown compressor")
				}
				if err != nil || cr.GetStatus().GetCode() != 0 || !proto.Equal(cr.Digest, b.digest) || !bytes.Equal(cd, b.data) {
					return fmt.Errorf("%s compressed batch mismatch", component)
				}
				if r.GetStatus().GetCode() != 0 || r.GetCompressor() != repb.Compressor_IDENTITY || !bytes.Equal(r.Data, b.data) || err != nil || !bytes.Equal(data, b.data) {
					return fmt.Errorf("%s bytes mismatch %s: %v", component, b.digest.Hash, err)
				}
			}
		}
		return nil
	}
	converge := func() {
		t.Helper()
		pollCtx, pollCancel := context.WithTimeout(ctx, 20*time.Second)
		defer pollCancel()
		started := time.Now()
		stable := 0
		var lastMismatch error
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			err := check(pollCtx)
			if err == nil {
				stable++
				if stable == 4 {
					t.Logf("convergence including stability scans: %s", time.Since(started))
					return
				}
			} else {
				stable = 0
				if pollCtx.Err() == nil {
					lastMismatch = err
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-pollCtx.Done():
				t.Fatalf("convergence deadline: %v", lastMismatch)
			case <-tick.C:
			}
		}
	}
	surfaces := func() {
		t.Helper()
		for _, b := range model {
			if !b.present {
				uri := "bytestream://app:1985/" + harness.ResourceName("", b.digest, false)
				missingData, err := getFile(ctx, clients[harness.App], uri)
				require.Empty(t, missingData)
				require.Equal(t, codes.Unavailable, status.Code(err), "pooled reader sanitizes missing errors")
				_, err = getRange(ctx, clients[harness.App], uri, b.digest.SizeBytes)
				require.Equal(t, codes.NotFound, status.Code(err))
				require.Equal(t, codes.NotFound, status.Code(grpcMetadata(ctx, clients[harness.App], b.digest)))
				metadataData, metadataCode, metadataErr := httpRead(ctx, f, "/rpc/BuildBuddyService/GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": b.digest.Hash, "sizeBytes": b.digest.SizeBytes}, "cacheType": 2, "digestFunction": 1}})
				require.NoError(t, metadataErr)
				require.NotEqual(t, 200, metadataCode)
				require.Contains(t, string(metadataData), "NotFound")
				for _, endpoint := range []string{"/api/v1/GetFile", "/api/v1/GetFileRange"} {
					body := map[string]any{"uri": uri}
					if endpoint == "/api/v1/GetFileRange" {
						body["end"] = b.digest.SizeBytes
					}
					data, code, err := httpRead(ctx, f, endpoint, body)
					require.NoError(t, err)
					require.NotEqual(t, 200, code, string(data))
					if endpoint == "/api/v1/GetFileRange" {
						require.Contains(t, string(data), "NotFound")
					} else {
						require.Equal(t, 404, code)
					}
				}
				for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
					cmd := exec.CommandContext(ctx, binary, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, fmt.Sprintf("%s/%d", b.digest.Hash, b.digest.SizeBytes))
					for _, e := range os.Environ() {
						if !strings.HasPrefix(e, "BUILDBUDDY_API_KEY=") {
							cmd.Env = append(cmd.Env, e)
						}
					}
					log, err := cmd.CombinedOutput()
					require.Error(t, err)
					require.Contains(t, string(log), "NotFound")
				}
				continue
			}
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				// URI host must resolve inside the app container, never a worker loopback.
				uri := "bytestream://" + strings.TrimPrefix(f.Endpoint(component).InternalGRPC, "grpc://") + "/" + harness.ResourceName("", b.digest, false)
				data, err := getFile(ctx, clients[harness.App], uri)
				require.NoError(t, err)
				require.True(t, bytes.Equal(b.data, data), "exact bytes")
				restData, restCode, restErr := httpRead(ctx, f, "/api/v1/GetFile", map[string]any{"uri": uri})
				require.NoError(t, restErr)
				require.Equal(t, 200, restCode, string(restData))
				require.True(t, bytes.Equal(b.data, restData))
				for _, path := range []string{"/file/download?bytestream_url=" + url.QueryEscape(uri), "/file/view?bytestream_url=" + url.QueryEscape(uri)} {
					data, code, err := httpRead(ctx, f, path, nil)
					require.NoError(t, err)
					require.Equal(t, 200, code, string(data))
					require.Equal(t, b.data, data, path)
				}
				out := filepath.Join(t.TempDir(), "blob")
				cmd := exec.CommandContext(ctx, binary, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+out, fmt.Sprintf("%s/%d", b.digest.Hash, b.digest.SizeBytes))
				// Never inherit the dispatch credential into the CLI client.
				for _, e := range os.Environ() {
					if !strings.HasPrefix(e, "BUILDBUDDY_API_KEY=") {
						cmd.Env = append(cmd.Env, e)
					}
				}
				log, err := cmd.CombinedOutput()
				require.NoError(t, err, string(log))
				data, err = os.ReadFile(out)
				require.NoError(t, err)
				require.True(t, bytes.Equal(b.data, data), "exact bytes")
			}
			if len(b.data) > 0 {
				for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
					assertMetadata(t, ctx, clients[peer], b.digest, "/cache.service.Cache/GetMetadata")
				}
				assertMetadata(t, ctx, clients[harness.App], b.digest, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata")
				if len(b.data) >= 11 {
					assertRanges(t, ctx, f, b)
				}
				uri := "bytestream://app:1985/" + harness.ResourceName("", b.digest, false)
				rangeData, rangeErr := getRange(ctx, clients[harness.App], uri, b.digest.SizeBytes)
				require.NoError(t, rangeErr)
				require.True(t, bytes.Equal(b.data, rangeData))
				require.NoError(t, grpcMetadata(ctx, clients[harness.App], b.digest))
				data, code, err := httpRead(ctx, f, "/api/v1/GetFileRange", map[string]any{"uri": uri, "start": 0, "end": b.digest.SizeBytes})
				require.NoError(t, err)
				require.Equal(t, 200, code, string(data))
				var response struct{ Data string }
				require.NoError(t, json.Unmarshal(data, &response))
				decoded, err := base64.StdEncoding.DecodeString(response.Data)
				require.NoError(t, err)
				require.Equal(t, b.data, decoded)
			}
			if len(b.data) == 0 {
				continue
			} // Empty CAS has virtual presence, no physical metadata.
			data, code, err := httpRead(ctx, f, "/rpc/BuildBuddyService/GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": b.digest.Hash, "sizeBytes": b.digest.SizeBytes}, "cacheType": 2, "digestFunction": 1}})
			require.NoError(t, err)
			require.Equal(t, 200, code, string(data))
			assertJSONMetadata(t, data, b.digest)
		}
	}
	stableSurfaces := func() {
		t.Helper()
		// Repeat the expensive HTTP/UI/CLI projections too, after the direct
		// convergence scans, without any intervening mutation.
		for scan := 0; scan < 2; scan++ {
			surfaces()
			if model[6].present {
				assertTrees(t, ctx, f, binary, rootDigest, []*repb.Directory{root, leaf}, int64(len(rootData)+len(leafData))+model[1].digest.SizeBytes)
			}
		}
	}
	converge()
	stableSurfaces()
	// Mix new and previously accepted blobs, then repeat each identical batch.
	for step, component := range []harness.Component{harness.App, harness.CacheProxy, harness.App, harness.CacheProxy, harness.App, harness.CacheProxy} {
		batch := []*blob{model[step+1], model[0]}
		if step > 0 {
			batch = append(batch, model[step])
		}
		for repeat := 0; repeat < 2; repeat++ {
			var requests []*repb.BatchUpdateBlobsRequest_Request
			for _, b := range batch {
				requests = append(requests, &repb.BatchUpdateBlobsRequest_Request{Digest: b.digest, Data: b.data})
			}
			res, err := clients[component].CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: requests, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, res.Responses, len(batch))
			accepted := map[string]bool{}
			for _, r := range res.Responses {
				require.EqualValues(t, 0, r.GetStatus().GetCode())
				accepted[r.GetDigest().GetHash()] = true
			}
			for _, b := range batch {
				require.True(t, accepted[b.digest.Hash])
				b.present = true
			}
			t.Logf("step=%d writer=%s repeat=%d", step, component, repeat)
			cold := new(wrapperspb.BytesValue)
			coldErr := clients[harness.CacheProxy].Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", metadataRequest(model[step+1].digest), cold)
			t.Logf("pre-read proxy local metadata: %v (local miss is not remote absence)", coldErr)
			converge()
			stableSurfaces()
		}
	}
	// CAS updates preserve bytes at a digest. Exercise the alternate streaming
	// mutation (identity and zstd) with the same model after every accepted write.
	for _, writer := range []harness.Component{harness.CacheProxy, harness.App} {
		for _, compressed := range []bool{false, true} {
			d, err := clients[writer].Write(ctx, "", model[1].data, compressed)
			require.NoError(t, err)
			require.True(t, proto.Equal(d, model[1].digest))
			t.Logf("ByteStream.Write writer=%s compressed=%v", writer, compressed)
			converge()
			stableSurfaces()
		}
	}

}

func TestAuditMetadataAndPartialRanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	for _, writer := range []harness.Component{harness.App, harness.CacheProxy} {
		data := []byte("audit partial-range and metadata " + string(writer))
		d := harness.Digest(data)
		res, err := f.Clients(writer).CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: data}}, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		require.Len(t, res.Responses, 1)
		require.EqualValues(t, 0, res.Responses[0].GetStatus().GetCode())
		for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
			got, err := f.Clients(peer).Read(ctx, "", d)
			require.NoError(t, err)
			require.Equal(t, data, got)
			digestWire := protowire.AppendTag(nil, 1, protowire.BytesType)
			digestWire = protowire.AppendString(digestWire, d.Hash)
			digestWire = protowire.AppendTag(digestWire, 2, protowire.VarintType)
			digestWire = protowire.AppendVarint(digestWire, uint64(d.SizeBytes))
			resource := protowire.AppendTag(nil, 1, protowire.BytesType)
			resource = protowire.AppendBytes(resource, digestWire)
			resource = protowire.AppendTag(resource, 4, protowire.VarintType)
			resource = protowire.AppendVarint(resource, 2)
			request := new(wrapperspb.BytesValue)
			wire := protowire.AppendTag(nil, 2, protowire.BytesType)
			wire = protowire.AppendBytes(wire, resource)
			request.ProtoReflect().SetUnknown(wire)
			response := new(wrapperspb.BytesValue)
			require.NoError(t, f.Clients(peer).Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", request, response), "peer=%s", peer)
			values := map[protowire.Number]uint64{}
			wire = response.ProtoReflect().GetUnknown()
			for len(wire) > 0 {
				num, typ, n := protowire.ConsumeTag(wire)
				require.Greater(t, n, 0)
				wire = wire[n:]
				if typ == protowire.VarintType {
					v, n := protowire.ConsumeVarint(wire)
					require.Greater(t, n, 0)
					values[num] = v
					wire = wire[n:]
				} else {
					n := protowire.ConsumeFieldValue(num, typ, wire)
					require.Greater(t, n, 0)
					wire = wire[n:]
				}
			}
			require.EqualValues(t, d.SizeBytes, values[5], "peer=%s digest size", peer)
			require.EqualValues(t, d.SizeBytes, values[2], "peer=%s stored size", peer)
			require.Greater(t, values[3], uint64(0))
			require.Greater(t, values[4], uint64(0))
			t.Logf("writer=%s peer=%s cache metadata=%v", writer, peer, values)
		}
		md, code, err := httpRead(ctx, f, "/rpc/BuildBuddyService/GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": d.SizeBytes}, "cacheType": 2, "digestFunction": 1}})
		require.NoError(t, err)
		require.Equal(t, 200, code, string(md))
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(md, &fields))
		for _, field := range []string{"digestSizeBytes", "storedSizeBytes", "lastAccessUsec", "lastModifyUsec"} {
			raw := strings.Trim(string(fields[field]), "\"")
			var v int64
			_, err := fmt.Sscan(raw, &v)
			require.NoError(t, err, "%s: %s", field, md)
			require.Greater(t, v, int64(0), "%s: %s", field, md)
			if field == "digestSizeBytes" || field == "storedSizeBytes" {
				require.Equal(t, d.SizeBytes, v, "%s", field)
			}
		}
		t.Logf("writer=%s metadata=%s", writer, md)
		for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
			uri := "bytestream://" + strings.TrimPrefix(f.Endpoint(peer).InternalGRPC, "grpc://") + "/" + harness.ResourceName("", d, false)
			req := wrapperspb.String(uri)
			wire := protowire.AppendTag(nil, 2, protowire.VarintType)
			wire = protowire.AppendVarint(wire, 3)
			wire = protowire.AppendTag(wire, 3, protowire.VarintType)
			wire = protowire.AppendVarint(wire, 11)
			req.ProtoReflect().SetUnknown(wire)
			out := new(wrapperspb.BytesValue)
			require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", req, out))
			require.Equal(t, data[3:11], out.Value)
			body, code, err := httpRead(ctx, f, "/api/v1/GetFileRange", map[string]any{"uri": uri, "start": 3, "end": 11})
			require.NoError(t, err)
			require.Equal(t, 200, code, string(body))
			var response struct{ Data string }
			require.NoError(t, json.Unmarshal(body, &response))
			decoded, err := base64.StdEncoding.DecodeString(response.Data)
			require.NoError(t, err)
			require.Equal(t, data[3:11], decoded)
		}
	}
}

func metadataRequest(d *repb.Digest) *wrapperspb.BytesValue {
	dw, _ := proto.Marshal(d)
	resource := protowire.AppendTag(nil, 1, protowire.BytesType)
	resource = protowire.AppendBytes(resource, dw)
	resource = protowire.AppendTag(resource, 4, protowire.VarintType)
	resource = protowire.AppendVarint(resource, 2)
	req := new(wrapperspb.BytesValue)
	req.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), resource))
	return req
}
func assertMetadata(t *testing.T, ctx context.Context, c *harness.Clients, d *repb.Digest, method string) {
	t.Helper()
	out := new(wrapperspb.BytesValue)
	require.NoError(t, c.Conn.Invoke(ctx, method, metadataRequest(d), out))
	values := map[protowire.Number]uint64{}
	wire := out.ProtoReflect().GetUnknown()
	for len(wire) > 0 {
		n, typ, k := protowire.ConsumeTag(wire)
		require.Greater(t, k, 0)
		wire = wire[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(wire)
			require.Greater(t, k, 0)
			values[n] = v
			wire = wire[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, wire)
			require.Greater(t, k, 0)
			wire = wire[k:]
		}
	}
	require.EqualValues(t, d.SizeBytes, values[5])
	require.Greater(t, values[2], uint64(0)) // stored representation may be compressed
	require.Greater(t, values[3], uint64(0))
	require.Greater(t, values[4], uint64(0))
}
func assertJSONMetadata(t *testing.T, data []byte, d *repb.Digest) {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	for _, field := range []string{"digestSizeBytes", "storedSizeBytes", "lastAccessUsec", "lastModifyUsec"} {
		var value int64
		_, err := fmt.Sscan(strings.Trim(string(fields[field]), "\""), &value)
		require.NoError(t, err, string(data))
		require.Greater(t, value, int64(0))
		if field == "digestSizeBytes" {
			require.Equal(t, d.SizeBytes, value)
		}
	}
}
func assertRanges(t *testing.T, ctx context.Context, f *harness.Fixture, b *blob) {
	t.Helper()
	for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
		c := f.Clients(peer)
		stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName("", b.digest, false), ReadOffset: 3, ReadLimit: 8})
		require.NoError(t, err)
		var data []byte
		for {
			r, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			data = append(data, r.Data...)
		}
		require.Equal(t, b.data[3:11], data)
		uri := "bytestream://" + strings.TrimPrefix(f.Endpoint(peer).InternalGRPC, "grpc://") + "/" + harness.ResourceName("", b.digest, false)
		req := wrapperspb.String(uri)
		wire := protowire.AppendVarint(protowire.AppendTag(nil, 2, protowire.VarintType), 3)
		wire = protowire.AppendVarint(protowire.AppendTag(wire, 3, protowire.VarintType), 11)
		req.ProtoReflect().SetUnknown(wire)
		out := new(wrapperspb.BytesValue)
		require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", req, out))
		require.Equal(t, b.data[3:11], out.Value)
		body, code, err := httpRead(ctx, f, "/api/v1/GetFileRange", map[string]any{"uri": uri, "start": 3, "end": 11})
		require.NoError(t, err)
		require.Equal(t, 200, code, string(body))
		var res struct{ Data string }
		require.NoError(t, json.Unmarshal(body, &res))
		decoded, err := base64.StdEncoding.DecodeString(res.Data)
		require.NoError(t, err)
		require.Equal(t, b.data[3:11], decoded)
	}
}

func assertTrees(t *testing.T, ctx context.Context, f *harness.Fixture, binary string, root *repb.Digest, expected []*repb.Directory, total int64) {
	t.Helper()
	expectedBytes := map[string]bool{}
	for _, d := range expected {
		b, err := proto.Marshal(d)
		require.NoError(t, err)
		expectedBytes[string(b)] = true
	}
	for _, peer := range []harness.Component{harness.App, harness.CacheProxy} {
		stream, err := f.Clients(peer).CAS.GetTree(ctx, &repb.GetTreeRequest{RootDigest: root, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		got := map[string]bool{}
		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			for _, d := range res.Directories {
				b, err := proto.Marshal(d)
				require.NoError(t, err)
				got[string(b)] = true
			}
		}
		require.Equal(t, expectedBytes, got)
		out := t.TempDir()
		cmd := exec.CommandContext(ctx, binary, "download", "--target=grpc://"+f.Endpoint(peer).GRPC, "--api_key="+f.APIKey, "--output_file="+filepath.Join(t.TempDir(), "root"), "--output_directory="+out, fmt.Sprintf("%s/%d", root.Hash, root.SizeBytes))
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "BUILDBUDDY_API_KEY=") {
				cmd.Env = append(cmd.Env, e)
			}
		}
		log, err := cmd.CombinedOutput()
		require.NoError(t, err, string(log))
		data, err := os.ReadFile(filepath.Join(out, "child", "payload"))
		require.NoError(t, err)
		require.Equal(t, []byte("app batch first\x00\xff"), data)
	}
	// UI streaming schema has root_digest=3 and directories=2.
	dw, err := proto.Marshal(root)
	require.NoError(t, err)
	req := new(wrapperspb.BytesValue)
	req.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), dw))
	stream, err := f.Clients(harness.App).Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree")
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(req))
	require.NoError(t, stream.CloseSend())
	got := map[string]bool{}
	for {
		res := new(wrapperspb.BytesValue)
		err := stream.RecvMsg(res)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		wire := res.ProtoReflect().GetUnknown()
		for len(wire) > 0 {
			n, typ, k := protowire.ConsumeTag(wire)
			require.Greater(t, k, 0)
			wire = wire[k:]
			if n == 2 && typ == protowire.BytesType {
				b, k := protowire.ConsumeBytes(wire)
				require.Greater(t, k, 0)
				got[string(b)] = true
				wire = wire[k:]
			} else {
				k := protowire.ConsumeFieldValue(n, typ, wire)
				require.Greater(t, k, 0)
				wire = wire[k:]
			}
		}
	}
	require.Equal(t, expectedBytes, got)
	// The frontend transports this same streaming method over prefixed HTTP.
	requestWire, err := proto.Marshal(req)
	require.NoError(t, err)
	framed := make([]byte, 5)
	binaryencoding.BigEndian.PutUint32(framed[1:], uint32(len(requestWire)))
	framed = append(framed, requestWire...)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/rpc/BuildBuddyService/GetTree", bytes.NewReader(framed))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/proto+prefixed")
	httpReq.Header.Set("x-buildbuddy-api-key", f.APIKey)
	httpRes, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	wireBody, err := io.ReadAll(httpRes.Body)
	httpRes.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, httpRes.StatusCode, string(wireBody))
	httpDirs := map[string]bool{}
	trailer := false
	for len(wireBody) > 0 {
		require.GreaterOrEqual(t, len(wireBody), 5)
		flags := wireBody[0]
		length := int(binaryencoding.BigEndian.Uint32(wireBody[1:5]))
		wireBody = wireBody[5:]
		require.GreaterOrEqual(t, len(wireBody), length)
		frame := wireBody[:length]
		wireBody = wireBody[length:]
		if flags&128 != 0 {
			trailer = true
			require.Contains(t, strings.ToLower(string(frame)), "grpc-status: 0")
			continue
		}
		for len(frame) > 0 {
			n, typ, k := protowire.ConsumeTag(frame)
			require.Greater(t, k, 0)
			frame = frame[k:]
			if n == 2 && typ == protowire.BytesType {
				b, k := protowire.ConsumeBytes(frame)
				require.Greater(t, k, 0)
				httpDirs[string(b)] = true
				frame = frame[k:]
			} else {
				k := protowire.ConsumeFieldValue(n, typ, frame)
				require.Greater(t, k, 0)
				frame = frame[k:]
			}
		}
	}
	require.True(t, trailer)
	require.Equal(t, expectedBytes, httpDirs)
	body, code, err := httpRead(ctx, f, "/rpc/BuildBuddyService/GetTreeDirectorySizes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "rootDigest": map[string]any{"hash": root.Hash, "sizeBytes": root.SizeBytes}, "digestFunction": 1})
	require.NoError(t, err)
	require.Equal(t, 200, code, string(body))
	var response struct {
		Sizes []struct {
			Digest     string
			TotalSize  json.RawMessage
			ChildCount json.RawMessage
		}
	}
	require.NoError(t, json.Unmarshal(body, &response))
	require.Len(t, response.Sizes, 2)
	// Check the unary gRPC projection of the same directory-size model.
	sizeReq := new(wrapperspb.BytesValue)
	sizeReq.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), dw))
	sizeRes := new(wrapperspb.BytesValue)
	require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", sizeReq, sizeRes))
	sizeWire := sizeRes.ProtoReflect().GetUnknown()
	grpcSizes := map[string][2]uint64{}
	for len(sizeWire) > 0 {
		n, typ, k := protowire.ConsumeTag(sizeWire)
		require.Greater(t, k, 0)
		sizeWire = sizeWire[k:]
		if n == 2 && typ == protowire.BytesType {
			entry, k := protowire.ConsumeBytes(sizeWire)
			require.Greater(t, k, 0)
			sizeWire = sizeWire[k:]
			var hash string
			var values [2]uint64
			for len(entry) > 0 {
				n, typ, k := protowire.ConsumeTag(entry)
				require.Greater(t, k, 0)
				entry = entry[k:]
				if n == 1 && typ == protowire.BytesType {
					v, k := protowire.ConsumeString(entry)
					require.Greater(t, k, 0)
					hash = v
					entry = entry[k:]
				} else if (n == 2 || n == 3) && typ == protowire.VarintType {
					v, k := protowire.ConsumeVarint(entry)
					require.Greater(t, k, 0)
					values[int(n)-2] = v
					entry = entry[k:]
				} else {
					k := protowire.ConsumeFieldValue(n, typ, entry)
					require.Greater(t, k, 0)
					entry = entry[k:]
				}
			}
			grpcSizes[hash] = values
		} else {
			k := protowire.ConsumeFieldValue(n, typ, sizeWire)
			require.Greater(t, k, 0)
			sizeWire = sizeWire[k:]
		}
	}
	require.Len(t, grpcSizes, 2)
	require.Equal(t, [2]uint64{uint64(total), 2}, grpcSizes[root.Hash])
	found := false
	for _, size := range response.Sizes {
		if size.Digest == root.Hash {
			found = true
			var v int64
			_, err := fmt.Sscan(strings.Trim(string(size.TotalSize), "\""), &v)
			require.NoError(t, err)
			require.Equal(t, total, v)
			require.Equal(t, "2", strings.Trim(string(size.ChildCount), "\""))
		}
	}
	require.True(t, found)
}

// A proxy is a read-through cache, not an asynchronously replicated store.
// Local metadata must remain absent before hydration, while forwarded
// presence must report the app's accepted write. Keep the two oracles separate.
func TestColdProxyAndInstance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	instance := "convergence-cas/nonempty"
	data := []byte("deterministic namespaced compressed valid write")
	compressed, err := harness.Compress(data)
	require.NoError(t, err)
	d := harness.Digest(data)
	res, err := app.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: compressed, Compressor: repb.Compressor_ZSTD}}, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Len(t, res.Responses, 1)
	require.EqualValues(t, 0, res.Responses[0].GetStatus().GetCode())
	req := metadataRequest(d)
	wire := req.ProtoReflect().GetUnknown()
	_, _, n := protowire.ConsumeTag(wire)
	resource, k := protowire.ConsumeBytes(wire[n:])
	require.Greater(t, k, 0)
	resource = protowire.AppendString(protowire.AppendTag(resource, 2, protowire.BytesType), instance)
	req.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), resource))
	started := time.Now()
	for i := 0; i < 5; i++ {
		missing, err := proxy.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Empty(t, missing.MissingBlobDigests)
		err = proxy.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", req, new(wrapperspb.BytesValue))
		require.Equal(t, codes.NotFound, status.Code(err), "no content read may hydrate this check")
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("cold local metadata stayed absent for %s; forwarded presence immediate", time.Since(started))
	for _, c := range []*harness.Clients{proxy, app} {
		got, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		require.Equal(t, data, got)
		out := new(wrapperspb.BytesValue)
		require.NoError(t, c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", req, out))
	}

}
