package convergence_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Supporting CAS uploads are checked immediately, not deferred until an AC
// validation happens to touch them. Canonical empty blobs require no stored
// object and have no valid nonempty GetFileRange request.
func checkSetupBlob(t *testing.T, ctx context.Context, f *harness.Fixture, instance string, d *repb.Digest, msg proto.Message) {
	t.Helper()
	expected, err := proto.Marshal(msg)
	require.NoError(t, err)
	bb, err := exec.LookPath("bb")
	require.NoError(t, err)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		c := f.Clients(component)
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Empty(t, missing.MissingBlobDigests)
		read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, read.Responses, 1)
		require.Equal(t, int32(0), read.Responses[0].GetStatus().GetCode())
		require.True(t, proto.Equal(d, read.Responses[0].Digest))
		require.True(t, bytes.Equal(expected, read.Responses[0].Data))
		body, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		require.True(t, bytes.Equal(expected, body))
		if d.SizeBytes > 0 {
			meta := &emptypb.Empty{}
			require.NoError(t, c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", wire(field(nil, 2, resource(d, instance, false))), meta))
			checkMetadata(t, meta.ProtoReflect().GetUnknown(), d.SizeBytes)
		}
		output := t.TempDir() + "/blob"
		log, err := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+output, "/"+harness.ResourceName(instance, d, false)).CombinedOutput()
		require.NoError(t, err, string(log))
		body, err = os.ReadFile(output)
		require.NoError(t, err)
		require.True(t, bytes.Equal(expected, body))
		uri := "bytestream://" + f.APIKey + "@" + strings.TrimPrefix(f.Endpoint(component).InternalGRPC, "grpc://") + "/" + harness.ResourceName(instance, d, false)
		app := f.Clients(harness.App)
		stream, err := app.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
		require.NoError(t, err)
		require.NoError(t, stream.SendMsg(wrapperspb.String(uri)))
		require.NoError(t, stream.CloseSend())
		body = nil
		for {
			out := &wrapperspb.BytesValue{}
			err = stream.RecvMsg(out)
			if err != nil {
				break
			}
			body = append(body, out.Value...)
		}
		require.Equal(t, io.EOF, err)
		require.True(t, bytes.Equal(expected, body))
		for _, path := range []string{"/file/download", "/file/view"} {
			req, err := http.NewRequestWithContext(ctx, "GET", f.Endpoint(harness.App).HTTP+path+"?bytestream_url="+url.QueryEscape(uri), nil)
			require.NoError(t, err)
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			res, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err = io.ReadAll(res.Body)
			res.Body.Close()
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode, string(body))
			require.True(t, bytes.Equal(expected, body))
		}
		for _, method := range []string{"GetFile", "GetFileRange"} {
			if d.SizeBytes == 0 && method == "GetFileRange" {
				continue
			} // No valid range exists for an empty blob.
			payload := map[string]any{"uri": uri}
			if method == "GetFileRange" {
				payload["end"] = fmt.Sprint(d.SizeBytes)
			}
			raw, err := json.Marshal(payload)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(raw))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			res, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err = io.ReadAll(res.Body)
			res.Body.Close()
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode, string(body))
			if method == "GetFileRange" {
				var result map[string]any
				require.NoError(t, json.Unmarshal(body, &result))
				require.Equal(t, base64.StdEncoding.EncodeToString(expected), result["data"])
			} else {
				require.True(t, bytes.Equal(expected, body))
			}
		}
	}
	if d.SizeBytes > 0 {
		meta := &emptypb.Empty{}
		require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", wire(field(nil, 2, resource(d, instance, false))), meta))
		checkMetadata(t, meta.ProtoReflect().GetUnknown(), d.SizeBytes)
		web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": "CAS"}})
		require.NoError(t, err)
		checkWebMetadata(t, web, d.SizeBytes)
	}
}
