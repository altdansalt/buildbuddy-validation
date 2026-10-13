package bytestreamstate

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
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
)

func httpBytes(t *testing.T, ctx context.Context, client *http.Client, endpoint, method string, body []byte, key string) []byte {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/protobuf")
	if key != "" {
		r.Header.Set("x-buildbuddy-api-key", key)
	}
	resp, err := client.Do(r)
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, string(b))
	return b
}

func checkExpanded(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, component harness.Component, instance string, data []byte, binary, key string, authoritativeMissing ...bool) {
	t.Helper()
	d := harness.Digest(data)
	missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
	require.NoError(t, err)
	if len(authoritativeMissing) > 0 && authoritativeMissing[0] {
		require.Len(t, missing.MissingBlobDigests, 1)
		require.True(t, proto.Equal(d, missing.MissingBlobDigests[0]))
	} else {
		require.Empty(t, missing.MissingBlobDigests)
	}
	batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Len(t, batch.Responses, 1)
	require.Zero(t, batch.Responses[0].Status.Code)
	require.True(t, proto.Equal(d, batch.Responses[0].Digest))
	require.Equal(t, repb.Compressor_IDENTITY, batch.Responses[0].Compressor)
	require.Equal(t, data, batch.Responses[0].Data)
	db, err := proto.Marshal(d)
	require.NoError(t, err)
	for _, compressor := range []uint64{0, 1} {
		rn := number(number(field(field(nil, 1, db), 2, []byte(instance)), 4, 2), 3, compressor)
		req := field(nil, 2, rn)
		md, err := unary(ctx, c, "/cache.service.Cache/GetMetadata", req)
		require.NoError(t, err)
		_, size := value(t, md, 5)
		require.Equal(t, uint64(len(data)), size)
		_, stored := value(t, md, 2)
		_, access := value(t, md, 3)
		_, modify := value(t, md, 4)
		require.Positive(t, stored)
		require.Positive(t, access)
		require.Positive(t, modify)
		// Stored representation and modification time must remain stable without a mutation.
		again, err := unary(ctx, c, "/cache.service.Cache/GetMetadata", req)
		require.NoError(t, err)
		_, stored2 := value(t, again, 2)
		_, modify2 := value(t, again, 4)
		require.Equal(t, stored, stored2)
		require.Equal(t, modify, modify2)
		if component == harness.App {
			ui, err := unary(ctx, c, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", req)
			require.NoError(t, err)
			browser := browserStream(t, ctx, f, "GetCacheMetadata", append(field(nil, 1, field(nil, 2, []byte(f.GroupID))), req...))
			require.Len(t, browser, 1)
			for _, n := range []int32{2, 4, 5} {
				_, a := value(t, md, protowire.Number(n))
				_, b := value(t, ui, protowire.Number(n))
				require.Equal(t, a, b)
				_, browserValue := value(t, browser[0], protowire.Number(n))
				require.Equal(t, a, browserValue)
			}
			web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": "CAS", "compressor": compressor}})
			require.NoError(t, err)
			require.Equal(t, fmt.Sprint(stored), fmt.Sprint(web["storedSizeBytes"]))
			require.Equal(t, fmt.Sprint(modify), fmt.Sprint(web["lastModifyUsec"]))
			require.NotEmpty(t, web["lastAccessUsec"])
		}
	}
	_, err = c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: harness.UploadResourceName(instance, d, false)})
	require.Equal(t, codes.Unimplemented, status.Code(err))
	for _, bounds := range [][2]int64{{0, 1}, {min(17, d.SizeBytes-1), min(1001, d.SizeBytes)}, {d.SizeBytes - 1, d.SizeBytes}, {0, d.SizeBytes}} {
		read, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, false), ReadOffset: bounds[0], ReadLimit: bounds[1] - bounds[0]})
		require.NoError(t, err)
		var got []byte
		for {
			r, e := read.Recv()
			if e == io.EOF {
				break
			}
			require.NoError(t, e)
			got = append(got, r.Data...)
		}
		require.Equal(t, data[bounds[0]:bounds[1]], got)
		compressedRead, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, true), ReadOffset: bounds[0], ReadLimit: bounds[1] - bounds[0]})
		require.NoError(t, err)
		var compressedBytes []byte
		for {
			r, e := compressedRead.Recv()
			if e == io.EOF {
				break
			}
			require.NoError(t, e)
			compressedBytes = append(compressedBytes, r.Data...)
		}
		decoded, err := harness.Decompress(compressedBytes)
		require.NoError(t, err)
		require.Equal(t, data[bounds[0]:bounds[1]], decoded)
		if component == harness.App {
			uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, false)
			req := number(number(field(nil, 1, []byte(uri)), 2, uint64(bounds[0])), 3, uint64(bounds[1]))
			got := httpBytes(t, ctx, http.DefaultClient, f.Endpoint(component).HTTP+"/api/v1/GetFileRange", "POST", req, key)
			got, _ = value(t, got, 1)
			require.Equal(t, data[bounds[0]:bounds[1]], got)
			got, err = unary(ctx, c, "/api.v1.ApiService/GetFileRange", req)
			require.NoError(t, err)
			got, _ = value(t, got, 1)
			require.Equal(t, data[bounds[0]:bounds[1]], got)
		}
	}
	if component == harness.App {
		for _, compressed := range []bool{false, true} {
			uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, compressed)
			got := httpBytes(t, ctx, http.DefaultClient, f.Endpoint(component).HTTP+"/api/v1/GetFile", "POST", field(nil, 1, []byte(uri)), key)
			if compressed {
				got, err = harness.Decompress(got)
				require.NoError(t, err)
			}
			require.Equal(t, data, got)
			requestContext := base64.StdEncoding.EncodeToString(field(nil, 2, []byte(f.GroupID)))
			for _, path := range []string{"/file/download", "/file/view"} {
				got := httpBytes(t, ctx, f.Web.Client, f.Web.URL+path+"?bytestream_url="+url.QueryEscape(uri)+"&request_context="+url.QueryEscape(requestContext), "GET", nil, "")
				if compressed {
					got, err = harness.Decompress(got)
					require.NoError(t, err)
				}
				require.Equal(t, data, got)
			}
		}
	}
	for _, compressed := range []bool{false, true} {
		cmd := exec.CommandContext(ctx, binary, "download", harness.ResourceName(instance, d, compressed), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+key)
		out, err := cmd.Output()
		require.NoError(t, err)
		require.Equal(t, data, out)
	}
}

// Local metadata intentionally does not read through. This pins the known
// cross-surface difference without misclassifying it as a product defect.
func TestColdMetadataDivergence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	data := bytes.Repeat([]byte("cold metadata state\n"), 3000)
	d, err := app.Write(ctx, "cold", data, true)
	require.NoError(t, err)
	db, err := proto.Marshal(d)
	require.NoError(t, err)
	req := field(nil, 2, number(field(field(nil, 1, db), 2, []byte("cold")), 4, 2))
	_, err = unary(ctx, app, "/cache.service.Cache/GetMetadata", req)
	require.NoError(t, err)
	start := time.Now()
	for time.Since(start) < 10*time.Second {
		_, err = unary(ctx, proxy, "/cache.service.Cache/GetMetadata", req)
		require.Equal(t, codes.NotFound, status.Code(err))
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("cold metadata divergence persisted %s without read-through", time.Since(start))
	start = time.Now()
	got, err := proxy.Read(ctx, "cold", d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	t.Logf("read-triggered metadata became available after %s", time.Since(start))
	for i := 0; i < 3; i++ {
		md, err := unary(ctx, proxy, "/cache.service.Cache/GetMetadata", req)
		require.NoError(t, err)
		_, size := value(t, md, 5)
		require.Equal(t, uint64(len(data)), size)
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("read-triggered metadata convergence stable after %s", time.Since(start))
}

func TestUnfinishedWrite(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	binary, err := exec.LookPath("bb")
	require.NoError(t, err)
	for _, writer := range []harness.Component{harness.App, harness.CacheProxy} {
		data := bytes.Repeat([]byte(string(writer)+"unfinished"), 1000)
		d := harness.Digest(data)
		c := f.Clients(writer)
		s, err := c.ByteStream.Write(ctx)
		require.NoError(t, err)
		require.NoError(t, s.Send(&bspb.WriteRequest{ResourceName: harness.UploadResourceName("unfinished", d, false), Data: data[:17]}))
		_, err = s.CloseAndRecv()
		require.Error(t, err)
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			c := f.Clients(component)
			checkAbsent(t, ctx, f, c, component, "unfinished", d, binary, f.APIKey)
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "unfinished", BlobDigests: []*repb.Digest{d}})
			require.NoError(t, err)
			require.Len(t, missing.MissingBlobDigests, 1)
			require.True(t, proto.Equal(d, missing.MissingBlobDigests[0]))
			_, err = c.Read(ctx, "unfinished", d)
			require.Equal(t, codes.NotFound, status.Code(err))
		}
		_, err = c.Write(ctx, "unfinished", data, false)
		require.NoError(t, err)
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			got, err := f.Clients(component).Read(ctx, "unfinished", d)
			require.NoError(t, err)
			require.Equal(t, data, got)
			checkExpanded(t, ctx, f, f.Clients(component), component, "unfinished", data, binary, f.APIKey)
		}
	}
}

func TestDeleteRecreateDivergence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary, err := exec.LookPath("bb")
	require.NoError(t, err)
	key2 := f.Web.CreateAPIKey(t, f.GroupID, "delete-client", []string{"CACHE_WRITE"})
	app := harness.Dial(t, f.Endpoint(harness.App).GRPC, key2)
	proxy := f.Clients(harness.CacheProxy)
	for _, writer := range []harness.Component{harness.App, harness.CacheProxy} {
		instance := "delete/" + string(writer)
		data := bytes.Repeat([]byte("delete recreate "+string(writer)), 3000)
		d, err := f.Clients(writer).Write(ctx, instance, data, false)
		require.NoError(t, err)
		for _, c := range []*harness.Clients{app, proxy} {
			got, err := c.Read(ctx, instance, d)
			require.NoError(t, err)
			require.Equal(t, data, got)
		}
		checkExpanded(t, ctx, f, app, harness.App, instance, data, binary, key2)
		checkExpanded(t, ctx, f, proxy, harness.CacheProxy, instance, data, binary, key2)
		uri := harness.ResourceName(instance, d, false)
		for repeat := 0; repeat < 2; repeat++ {
			_, err = unary(ctx, app, "/api.v1.ApiService/DeleteFile", field(nil, 1, []byte(uri)))
			require.NoError(t, err)
			_, err = app.Read(ctx, instance, d)
			require.Equal(t, codes.NotFound, status.Code(err))
			checkAbsent(t, ctx, f, app, harness.App, instance, d, binary, key2)
			checkExpanded(t, ctx, f, proxy, harness.CacheProxy, instance, data, binary, key2, true)
			// A warmed local proxy retains bytes after app-local deletion. Assert the
			// known difference rather than silently letting a read republish it.
			start := time.Now()
			for time.Since(start) < 2*time.Second {
				got, err := proxy.Read(ctx, instance, d)
				require.NoError(t, err)
				require.Equal(t, data, got)
				missing, err := app.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
				require.NoError(t, err)
				require.Len(t, missing.MissingBlobDigests, 1)
				time.Sleep(200 * time.Millisecond)
			}
			t.Logf("writer=%s delete=%d stale proxy persisted %s", writer, repeat, time.Since(start))
		}
		// A valid repeat Write through the proxy re-creates the authoritative blob.
		recreated := time.Now()
		_, err = proxy.Write(ctx, instance, data, false)
		require.NoError(t, err)
		checkExpanded(t, ctx, f, app, harness.App, instance, data, binary, key2)
		checkExpanded(t, ctx, f, proxy, harness.CacheProxy, instance, data, binary, key2)
		t.Logf("writer=%s proxy re-create observed on every file surface after %s", writer, time.Since(recreated))
		// Delete again before re-creating from the second app client.
		_, err = unary(ctx, app, "/api.v1.ApiService/DeleteFile", field(nil, 1, []byte(uri)))
		require.NoError(t, err)
		checkAbsent(t, ctx, f, app, harness.App, instance, d, binary, key2)
		checkExpanded(t, ctx, f, proxy, harness.CacheProxy, instance, data, binary, key2, true)
		// Re-create on the authoritative app and require stable replica contents.
		_, err = app.Write(ctx, instance, data, false)
		require.NoError(t, err)
		checkExpanded(t, ctx, f, app, harness.App, instance, data, binary, key2)
		checkExpanded(t, ctx, f, proxy, harness.CacheProxy, instance, data, binary, key2)
		for stable := 0; stable < 3; stable++ {
			for _, c := range []*harness.Clients{app, proxy} {
				got, err := c.Read(ctx, instance, d)
				require.NoError(t, err)
				require.Equal(t, data, got)
			}
		}
	}
}

// Browser transport uses gRPC-framed protobuf over HTTP, not the JSON protolet.
func browserStream(t *testing.T, ctx context.Context, f *harness.Fixture, method string, req []byte) [][]byte {
	t.Helper()
	return browserFrames(t, ctx, f, method, req, 0)
}
func browserFrames(t *testing.T, ctx context.Context, f *harness.Fixture, method string, req []byte, expectedCode int) [][]byte {
	t.Helper()
	framed := make([]byte, 5)
	binary.BigEndian.PutUint32(framed[1:], uint32(len(req)))
	framed = append(framed, req...)
	r, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(framed))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/proto+prefixed")
	resp, err := f.Web.Client.Do(r)
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, string(b))
	var messages [][]byte
	ok := resp.Header.Get("grpc-status") == fmt.Sprint(expectedCode)
	for len(b) > 0 {
		require.GreaterOrEqual(t, len(b), 5)
		flags := b[0]
		n := int(binary.BigEndian.Uint32(b[1:5]))
		b = b[5:]
		require.GreaterOrEqual(t, len(b), n)
		if flags == 0x80 {
			require.Contains(t, strings.ToLower(string(b[:n])), fmt.Sprintf("grpc-status: %d", expectedCode))
			ok = true
		} else {
			require.Zero(t, flags)
			messages = append(messages, append([]byte(nil), b[:n]...))
		}
		b = b[n:]
	}
	require.True(t, ok, "browser stream must end with expected grpc-status")
	return messages
}

func checkUITree(t *testing.T, ctx context.Context, f *harness.Fixture, instance string, root *repb.Digest, directory *repb.Directory) {
	t.Helper()
	db, err := proto.Marshal(root)
	require.NoError(t, err)
	req := field(field(field(nil, 1, field(nil, 2, []byte(f.GroupID))), 2, []byte(instance)), 3, db)
	c := f.Clients(harness.App)
	s, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	require.NoError(t, s.SendMsg(&req))
	require.NoError(t, s.CloseSend())
	count := 0
	for {
		var out []byte
		err = s.RecvMsg(&out)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		raw, _ := value(t, out, 2)
		if len(raw) > 0 {
			var d repb.Directory
			require.NoError(t, proto.Unmarshal(raw, &d))
			require.True(t, proto.Equal(directory, &d))
			count++
		}
	}
	require.Equal(t, 1, count)
	count = 0
	for _, out := range browserStream(t, ctx, f, "GetTree", req) {
		raw, _ := value(t, out, 2)
		if len(raw) > 0 {
			var d repb.Directory
			require.NoError(t, proto.Unmarshal(raw, &d))
			require.True(t, proto.Equal(directory, &d))
			count++
		}
	}
	require.Equal(t, 1, count)
	sizesReq := field(field(field(nil, 1, field(nil, 2, []byte(f.GroupID))), 2, db), 3, []byte(instance))
	out, err := unary(ctx, c, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", sizesReq)
	require.NoError(t, err)
	browser := browserStream(t, ctx, f, "GetTreeDirectorySizes", sizesReq)
	require.Len(t, browser, 1)
	require.Equal(t, out, browser[0])
	row, _ := value(t, out, 2)
	hash, _ := value(t, row, 1)
	require.Equal(t, root.Hash, string(hash))
	_, total := value(t, row, 2)
	require.Equal(t, uint64(root.SizeBytes+directory.Files[0].Digest.SizeBytes), total)
}

// Absence is checked independently; no read surface may publish partial bytes.
func checkAbsent(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, component harness.Component, instance string, d *repb.Digest, binary, key string) {
	t.Helper()
	for _, z := range []bool{false, true} {
		rn := harness.ResourceName(instance, d, z)
		s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: rn})
		if err == nil {
			_, err = s.Recv()
		}
		require.Equal(t, codes.NotFound, status.Code(err))
		cmd := exec.CommandContext(ctx, binary, "download", rn, "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+key)
		out, err := cmd.Output()
		require.Error(t, err)
		require.Empty(t, out)
		db, err := proto.Marshal(d)
		require.NoError(t, err)
		compressor := uint64(0)
		if z {
			compressor = 1
		}
		req := field(nil, 2, number(number(field(field(nil, 1, db), 2, []byte(instance)), 4, 2), 3, compressor))
		_, err = unary(ctx, c, "/cache.service.Cache/GetMetadata", req)
		require.Equal(t, codes.NotFound, status.Code(err))
		if component != harness.App {
			continue
		}
		_, err = unary(ctx, c, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", req)
		require.Equal(t, codes.NotFound, status.Code(err))
		frames := browserFrames(t, ctx, f, "GetCacheMetadata", append(field(nil, 1, field(nil, 2, []byte(f.GroupID))), req...), int(codes.NotFound))
		require.Empty(t, frames)
		_, err = f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": "CAS", "compressor": compressor}})
		require.Error(t, err)
		uri := "bytestream://app:1985/" + rn
		out, err = stream(ctx, c, "/api.v1.ApiService/GetFile", field(nil, 1, []byte(uri)))
		require.Equal(t, codes.Unavailable, status.Code(err)) // pooled client sanitizes exhausted missing reads
		require.Empty(t, out)
		if !z {
			_, err = unary(ctx, c, "/api.v1.ApiService/GetFileRange", number(field(nil, 1, []byte(uri)), 3, 1))
			require.Equal(t, codes.NotFound, status.Code(err))
		}
		for _, path := range []string{"/file/download", "/file/view", "/api/v1/GetFile", "/api/v1/GetFileRange"} {
			method, body, target := "GET", []byte(nil), f.Web.URL+path+"?bytestream_url="+url.QueryEscape(uri)+"&request_context="+url.QueryEscape(base64.StdEncoding.EncodeToString(field(nil, 2, []byte(f.GroupID))))
			if strings.HasPrefix(path, "/api/") {
				method = "POST"
				body = field(nil, 1, []byte(uri))
				if strings.HasSuffix(path, "Range") {
					body = number(body, 3, 1)
				}
				target = f.Web.URL + path
			}
			r, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
			require.NoError(t, err)
			r.Header.Set("Content-Type", "application/protobuf")
			r.Header.Set("x-buildbuddy-api-key", key)
			resp, err := f.Web.Client.Do(r)
			require.NoError(t, err)
			body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			if path == "/api/v1/GetFileRange" {
				require.Equal(t, http.StatusInternalServerError, resp.StatusCode, path)
				require.Contains(t, string(body), "NotFound")
			} else if path == "/api/v1/GetFile" {
				require.Equal(t, http.StatusNotFound, resp.StatusCode, path)
				require.Contains(t, string(body), "failed to read byte stream")
			} else {
				// Direct bytestream URLs need no invocation for existing blobs. After a
				// missing read, the UI handler tries its invocation-blobstore fallback.
				require.Equal(t, http.StatusBadRequest, resp.StatusCode, path)
				require.Contains(t, string(body), "Missing invocation_id param")
			}
		}
	}
	batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Len(t, batch.Responses, 1)
	require.Equal(t, int32(codes.NotFound), batch.Responses[0].Status.Code)
	require.True(t, proto.Equal(d, batch.Responses[0].Digest))
	require.Empty(t, batch.Responses[0].Data)
	missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Len(t, missing.MissingBlobDigests, 1)
	require.True(t, proto.Equal(d, missing.MissingBlobDigests[0]))
}

// ZIP viewers expose derived file state through a distinct metadata RPC and
// download selector. Use a valid deterministic archive, not opaque blob bytes.
func TestZipPublication(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	binary, err := exec.LookPath("bb")
	require.NoError(t, err)
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	content := []byte("published ZIP member\n")
	w, err := z.CreateHeader(&zip.FileHeader{Name: "logs/output.txt", Method: zip.Deflate})
	require.NoError(t, err)
	_, err = w.Write(content)
	require.NoError(t, err)
	require.NoError(t, z.Close())
	for _, writer := range []harness.Component{harness.App, harness.CacheProxy} {
		instance := "zip/" + string(writer)
		for _, compressed := range []bool{false, true} {
			d, err := f.Clients(writer).Write(ctx, instance, archive.Bytes(), compressed)
			require.NoError(t, err)
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				_, err := f.Clients(component).Read(ctx, instance, d)
				require.NoError(t, err)
				checkExpanded(t, ctx, f, f.Clients(component), component, instance, archive.Bytes(), binary, f.APIKey)
			}
			uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, false)
			req := field(field(nil, 1, field(nil, 2, []byte(f.GroupID))), 2, []byte(uri))
			out, err := unary(ctx, f.Clients(harness.App), "/buildbuddy.service.BuildBuddyService/GetZipManifest", req)
			require.NoError(t, err)
			browser := browserStream(t, ctx, f, "GetZipManifest", req)
			require.Len(t, browser, 1)
			require.Equal(t, out, browser[0])
			manifest, _ := value(t, out, 2)
			entry, _ := value(t, manifest, 1)
			name, _ := value(t, entry, 1)
			require.Equal(t, "logs/output.txt", string(name))
			_, size := value(t, entry, 2)
			require.Equal(t, uint64(len(content)), size)
			web, err := f.Web.RPC("GetZipManifest", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "uri": uri})
			require.NoError(t, err)
			webManifest, ok := web["manifest"].(map[string]any)
			require.True(t, ok, "%v", web)
			entries, ok := webManifest["entry"].([]any)
			require.True(t, ok, "%v", webManifest)
			require.Len(t, entries, 1)
			webEntry := entries[0].(map[string]any)
			require.Equal(t, "logs/output.txt", webEntry["name"])
			require.Equal(t, fmt.Sprint(len(content)), fmt.Sprint(webEntry["uncompressedSize"]))
			for _, path := range []string{"/file/download", "/file/view"} {
				target := f.Web.URL + path + "?bytestream_url=" + url.QueryEscape(uri) + "&z=" + url.QueryEscape(base64.StdEncoding.EncodeToString(entry)) + "&request_context=" + url.QueryEscape(base64.StdEncoding.EncodeToString(field(nil, 2, []byte(f.GroupID))))
				got := httpBytes(t, ctx, f.Web.Client, target, "GET", nil, "")
				require.Equal(t, content, got)
			}
		}
	}
}

// GetFileRange documents compressed URIs, but does not use ByteStream's range
// transcoding. Pin this known difference with supported identity/BS controls.
func TestCompressedAPIRangeDivergence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, writer := range []harness.Component{harness.App, harness.CacheProxy} {
		data := bytes.Repeat([]byte("compressed range state\n"), 3000)
		instance := "compressed-range/" + string(writer)
		d, err := f.Clients(writer).Write(ctx, instance, data, true)
		require.NoError(t, err)
		c := f.Clients(harness.App)
		uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, false)
		req := number(number(field(nil, 1, []byte(uri)), 2, 17), 3, 1001)
		out, err := unary(ctx, c, "/api.v1.ApiService/GetFileRange", req)
		require.NoError(t, err)
		got, _ := value(t, out, 1)
		require.Equal(t, data[17:1001], got)
		s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, true), ReadOffset: 17, ReadLimit: 984})
		require.NoError(t, err)
		var encoded []byte
		for {
			r, err := s.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			encoded = append(encoded, r.Data...)
		}
		got, err = harness.Decompress(encoded)
		require.NoError(t, err)
		require.Equal(t, data[17:1001], got)
		uri = "bytestream://app:1985/" + harness.ResourceName(instance, d, true)
		req = number(number(field(nil, 1, []byte(uri)), 2, 17), 3, 1001)
		start := time.Now()
		for stable := 0; stable < 3; stable++ {
			_, err = unary(ctx, c, "/api.v1.ApiService/GetFileRange", req)
			require.Equal(t, codes.FailedPrecondition, status.Code(err))
			require.Contains(t, err.Error(), "passthrough compression does not support offset/limit")
			time.Sleep(100 * time.Millisecond)
		}
		r, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/GetFileRange", bytes.NewReader(req))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/protobuf")
		r.Header.Set("x-buildbuddy-api-key", f.APIKey)
		resp, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		require.Contains(t, string(body), "passthrough compression does not support offset/limit")
		t.Logf("writer=%s compressed API range remains rejected after %s; identity API and compressed ByteStream ranges succeed", writer, time.Since(start))
	}
}
