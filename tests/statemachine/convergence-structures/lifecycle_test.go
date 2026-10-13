package structures_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Continue through the documented delete boundary to exercise repeated delete
// and re-creation. The isolated bug test asserts the desired proxy invalidation.
func TestDeleteRecreateSurfaceBoundary(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	second := f.Web.CreateAPIKey(t, f.GroupID, "delete-second", []string{"CACHE_WRITE"})
	cs := []*harness.Clients{f.Clients(harness.App), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, second)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	iid := publishDownloadInvocation(t, ctx, cs[0])
	m := &model{blobs: map[string][]byte{}, dirs: map[string]*repb.Directory{}, mappings: map[string][]*repb.Digest{}}
	for side, c := range cs {
		content := bytes.Repeat([]byte{byte(65 + side)}, 128)
		d := m.put(t, ctx, c, content, side == 1)
		converge(t, m, f, cs)
		for repeat := 0; repeat < 2; repeat++ {
			var raw []byte
			require.NoError(t, cs[0].Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", field(nil, 1, []byte("blobs/"+key(d))), &raw, grpc.ForceCodec(wireCodec{})))
			delete(m.blobs, key(d))
			started := time.Now()
			checkDeletedBoundary(t, ctx, f, cs, d, content, iid)
			t.Logf("delete boundary observed in %s", time.Since(started))
			converge(t, m, f, cs)
			time.Sleep(100 * time.Millisecond)
			checkDeletedBoundary(t, ctx, f, cs, d, content, iid)
		}
		// Re-create through the opposite endpoint, then repeat the valid publication.
		for repeat := 0; repeat < 2; repeat++ {
			m.put(t, ctx, cs[1-side], content, repeat == 1)
			converge(t, m, f, cs)
		}
	}
}
func checkDeletedBoundary(t *testing.T, ctx context.Context, f *harness.Fixture, cs []*harness.Clients, d *repb.Digest, content []byte, iid string) {
	t.Helper()
	dg, _ := proto.Marshal(d)
	rn := number(number(field(nil, 1, dg), 4, 2), 5, 1)
	for side, c := range cs {
		got, err := c.Read(ctx, instance, d)
		if side == 0 {
			require.Equal(t, codes.NotFound, status.Code(err))
		} else {
			require.NoError(t, err)
			require.Equal(t, content, got, "known stale proxy boundary")
		}
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		require.Len(t, missing.MissingBlobDigests, 1)
		require.True(t, proto.Equal(d, missing.MissingBlobDigests[0]))
		batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		require.Len(t, batch.Responses, 1)
		require.True(t, proto.Equal(d, batch.Responses[0].Digest))
		if side == 0 {
			require.EqualValues(t, codes.NotFound, batch.Responses[0].GetStatus().GetCode())
		} else {
			require.Zero(t, batch.Responses[0].GetStatus().GetCode())
			require.Equal(t, content, batch.Responses[0].Data)
		}
		for _, compressed := range []bool{false, true} {
			req := field(nil, 1, []byte(harness.ResourceName(instance, d, compressed)))
			if !compressed {
				req = number(number(req, 2, 1), 3, 10)
			}
			got, err := streamBytes(ctx, c, "/google.bytestream.ByteStream/Read", req)
			if side == 0 {
				require.Equal(t, codes.NotFound, status.Code(err))
			} else {
				require.NoError(t, err)
				if compressed {
					got, err = harness.Decompress(got)
					require.NoError(t, err)
					require.Equal(t, content, got)
				} else {
					require.Equal(t, content[1:11], got)
				}
			}
		}
		var raw []byte
		err = c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", field(nil, 2, rn), &raw, grpc.ForceCodec(wireCodec{}))
		if side == 0 {
			require.Equal(t, codes.NotFound, status.Code(err))
		} else {
			require.NoError(t, err)
			size, err := scalar(raw, 5)
			require.NoError(t, err)
			require.EqualValues(t, len(content), size)
		}
		bb, err := exec.LookPath("bb")
		require.NoError(t, err)
		out := t.TempDir()
		dest := filepath.Join(out, "blob")
		component := harness.App
		if side == 1 {
			component = harness.CacheProxy
		}
		cmd := exec.CommandContext(ctx, bb, "download", key(d), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+dest)
		cmd.Env = []string{"HOME=" + out, "PATH=/usr/local/bin:/usr/bin:/bin"}
		log, err := cmd.CombinedOutput()
		if side == 0 {
			require.Error(t, err, string(log))
		} else {
			require.NoError(t, err, string(log))
			got, err := os.ReadFile(dest)
			require.NoError(t, err)
			require.Equal(t, content, got)
		}
	}
	uri := "bytestream://app:1985/blobs/" + key(d)
	_, err := streamBytes(ctx, cs[0], "/api.v1.ApiService/GetFile", field(nil, 1, []byte(uri)))
	require.Equal(t, codes.Unavailable, status.Code(err), "public GetFile sanitizes absent-resource errors")
	var raw []byte
	err = cs[0].Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", number(field(nil, 1, []byte(uri)), 3, 10), &raw, grpc.ForceCodec(wireCodec{}))
	require.Equal(t, codes.NotFound, status.Code(err))
	err = cs[0].Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", field(nil, 2, rn), &raw, grpc.ForceCodec(wireCodec{}))
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": d.SizeBytes}, "cacheType": "CAS", "digestFunction": "SHA256"}})
	require.Error(t, err)
	for _, method := range []string{"GetFile", "GetFileRange"} {
		payload := map[string]any{"uri": uri}
		if method == "GetFileRange" {
			payload["end"] = 10
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/"+method, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		rsp, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(rsp.Body)
		rsp.Body.Close()
		require.NoError(t, err)
		if method == "GetFile" {
			require.Equal(t, 404, rsp.StatusCode)
		} else {
			require.Equal(t, 500, rsp.StatusCode)
		}
		if method == "GetFile" {
			require.Contains(t, string(data), "failed to read byte stream resource")
		} else {
			require.Contains(t, string(data), "NotFound")
		}
	}
	for _, path := range []string{"/file/download", "/file/view"} {
		q := url.Values{"invocation_id": {iid}, "bytestream_url": {uri}, "request_context": {base64.StdEncoding.EncodeToString(field(nil, 2, []byte(f.GroupID)))}}
		req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+path+"?"+q.Encode(), nil)
		require.NoError(t, err)
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		rsp, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(rsp.Body)
		rsp.Body.Close()
		require.NoError(t, err)
		// The fixture disk blobstore returns a plain filesystem missing error;
		// fallback sanitizes it to Internal. Require absence of payload and the
		// exact released error, rather than treating it as cached content.
		require.Equal(t, http.StatusInternalServerError, rsp.StatusCode, string(data))
		require.Contains(t, string(data), "Internal server error")
	}
}

// The frontend includes invocation_id; provision a real completed local build
// so missing-CAS fallback uses a valid invocation and not a missing parameter.
func publishDownloadInvocation(t *testing.T, ctx context.Context, c *harness.Clients) string {
	t.Helper()
	iid := "10101010-1234-4567-9876-111111111111"
	s, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	started := field(field(nil, 1, field(nil, 3, nil)), 5, field(field(nil, 3, []byte("8.0.0")), 5, []byte("build")))
	finished := number(field(field(nil, 1, field(nil, 9, nil)), 14, field(nil, 3, field(nil, 1, []byte("SUCCESS")))), 20, 1)
	for i, event := range [][]byte{started, finished} {
		require.NoError(t, s.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: &bespb.StreamId{InvocationId: iid, BuildId: iid, Component: bespb.StreamId_TOOL}, SequenceNumber: int64(i + 1), Event: &bespb.BuildEvent{Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: event}}}}}))
	}
	require.NoError(t, s.CloseSend())
	acks := 0
	for {
		_, err := s.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		acks++
	}
	require.Equal(t, 2, acks)
	return iid
}
