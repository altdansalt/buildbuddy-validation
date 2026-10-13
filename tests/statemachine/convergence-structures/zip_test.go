package structures_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestZipContentOffset(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var blob bytes.Buffer
	writer := zip.NewWriter(&blob)
	file, err := writer.Create("payload.txt")
	require.NoError(t, err)
	_, err = file.Write([]byte("ZIP offset conformance"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	c := f.Clients(harness.App)
	d, err := c.Upload(ctx, instance, blob.Bytes())
	require.NoError(t, err)
	archive, err := zip.NewReader(bytes.NewReader(blob.Bytes()), int64(blob.Len()))
	require.NoError(t, err)
	offset, err := archive.File[0].DataOffset()
	require.NoError(t, err)
	uri := "bytestream://app:1985/blobs/" + key(d)
	var raw []byte
	require.NoError(t, c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetZipManifest", field(nil, 2, []byte(uri)), &raw, grpc.ForceCodec(wireCodec{})))
	manifests, err := fields(raw, 2)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	entries, err := fields(manifests[0], 1)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	q := url.Values{"bytestream_url": {uri}, "z": {base64.StdEncoding.EncodeToString(entries[0])}, "request_context": {base64.StdEncoding.EncodeToString(field(nil, 2, []byte(f.GroupID)))}}
	req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?"+q.Encode(), nil)
	require.NoError(t, err)
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	rsp, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	data, err := io.ReadAll(rsp.Body)
	rsp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rsp.StatusCode)
	require.Equal(t, []byte("ZIP offset conformance"), data)
	actual, err := scalar(entries[0], 5)
	require.NoError(t, err)
	// Positive control: the supplied returned entry successfully extracts bytes.
	// Both UI metadata transports must also expose the true content offset.
	result, err := f.Web.RPC("GetZipManifest", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "uri": uri})
	require.NoError(t, err)
	manifest := result["manifest"].(map[string]any)
	entry := manifest["entry"].([]any)[0].(map[string]any)
	if int64(actual) != offset {
		t.Errorf("gRPC content_offset=%d want actual ZIP data offset %d", actual, offset)
	}
	if jsonInteger(entry["contentOffset"]) != jsonInteger(offset) {
		t.Errorf("HTTP contentOffset=%v want actual ZIP data offset %d", entry["contentOffset"], offset)
	}
}
