package public_cache_delete_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// The artifact browser adds a ZIP manifest reader and ranged member downloads.
func TestZipDeleteSequence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	invocation := "b7b751bd-2619-40ba-ad97-201e89296c23"
	publishInvocation(t, ctx, c, invocation)
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, e := zw.CreateHeader(&zip.FileHeader{Name: "member.txt", Method: zip.Store})
	require.NoError(t, e)
	payload := []byte("zip member payload")
	_, e = w.Write(payload)
	require.NoError(t, e)
	require.NoError(t, zw.Close())
	blob := archive.Bytes()
	d := harness.Digest(blob)
	resource := harness.ResourceName("", d, false)
	uri := "bytestream://" + f.Endpoint(harness.App).InternalGRPC[len("grpc://"):] + "/" + resource
	present := false
	var retainedEntry []byte
	check := func() {
		req := wire(field(2, []byte(uri)))
		var res wire
		e := retryUnavailable(ctx, func() error {
			return c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetZipManifest", &req, &res, grpc.ForceCodec(codec{}))
		})
		if present {
			require.NoError(t, e)
			manifests := byteFields(res, 2)
			require.Len(t, manifests, 1)
			entries := byteFields(manifests[0], 1)
			require.Len(t, entries, 1)
			require.Equal(t, [][]byte{[]byte("member.txt")}, byteFields(entries[0], 1))
			require.Equal(t, uint64(len(payload)), varints(entries[0])[2])
			require.Equal(t, uint64(len(payload)), varints(entries[0])[3])
			retainedEntry = entries[0]
			messages, code := webWire(t, ctx, f, "GetZipManifest", req)
			require.Equal(t, codes.OK, code)
			require.Equal(t, [][]byte{res}, messages)
		} else {
			require.Equal(t, codes.Unavailable, status.Code(e))
			messages, code := webWire(t, ctx, f, "GetZipManifest", req)
			require.Equal(t, codes.Unavailable, code)
			require.Empty(t, messages)
		}
		got, e := c.Read(ctx, "", d)
		if present {
			require.NoError(t, e)
			require.Equal(t, blob, got)
		} else {
			require.Equal(t, codes.NotFound, status.Code(e))
		}
		if len(retainedEntry) > 0 {
			for _, route := range []string{"view", "download"} {
				r, e := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/"+route+"?invocation_id="+invocation+"&bytestream_url="+url.QueryEscape(uri)+"&z="+url.QueryEscape(base64.StdEncoding.EncodeToString(retainedEntry)), nil)
				require.NoError(t, e)
				r.Header.Set("x-buildbuddy-api-key", f.APIKey)
				resp, e := f.Web.Client.Do(r)
				require.NoError(t, e)
				b, e := io.ReadAll(resp.Body)
				resp.Body.Close()
				require.NoError(t, e)
				if present {
					require.Equal(t, 200, resp.StatusCode, string(b))
					require.Equal(t, payload, b)
				} else {
					require.Equal(t, 404, resp.StatusCode, string(b))
				}
			}
		}
	}
	// Avoid startup retry on an intentionally missing ZIP, which is also Unavailable.
	_, e = c.Upload(ctx, "", blob)
	require.NoError(t, e)
	present = true
	check()
	_, e = c.Write(ctx, "", blob, false)
	require.NoError(t, e)
	check()
	req := wire(field(1, []byte(resource)))
	var res wire
	require.NoError(t, c.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &req, &res, grpc.ForceCodec(codec{})))
	present = false
	check()
	_, e = c.Write(ctx, "", blob, true)
	require.NoError(t, e)
	present = true
	check()
}
