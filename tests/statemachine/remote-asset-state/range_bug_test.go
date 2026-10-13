package remoteassetstate

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	rapb "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

// Positive agreement contract; excluded from green CI with the bug tag.
func fetchedProxyAsset(t *testing.T, want []byte) (*harness.Fixture, context.Context, *repb.Digest) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	origin := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write(want)
		if err != nil {
			t.Errorf("origin write: %v", err)
		}
	})}
	go origin.Serve(listener)
	t.Cleanup(func() { origin.Close() })
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.CacheProxy: {"--remote_asset.allowed_private_ips=0.0.0.0/0"}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	uri := fmt.Sprintf("http://%s:%d/asset", fixtureGateway(t, f), listener.Addr().(*net.TCPAddr).Port)
	rsp, err := rapb.NewFetchClient(f.Clients(harness.CacheProxy).Conn).FetchBlob(ctx, &rapb.FetchBlobRequest{Uris: []string{uri}})
	require.NoError(t, err)
	require.EqualValues(t, 0, rsp.GetStatus().GetCode())
	return f, ctx, rsp.BlobDigest
}

func TestProxyFetchRangeAgreement(t *testing.T) {
	want := []byte("isolated proxy range asset\n")
	f, ctx, d := fetchedProxyAsset(t, want)
	c := f.Clients(harness.App)
	checkProxyFileReaders(t, ctx, f, c, "", d, want)
	req := bytesField(1, []byte("bytestream://cacheproxy:1985/"+harness.ResourceName("", d, false)))
	req = append(req, intField(2, 1)...)
	req = append(req, intField(3, uint64(len(want)-1))...)
	out := unary(t, ctx, c, "/api.v1.ApiService/GetFileRange", req)
	got, _ := field(out, 1)
	require.Equal(t, want[1:len(want)-1], got)
}

func TestProxyFetchTreeAgreement(t *testing.T) {
	dir := &repb.Directory{Files: []*repb.FileNode{{Name: "file", Digest: harness.Digest([]byte("file"))}}}
	want, d, err := harness.MarshalDigest(dir)
	require.NoError(t, err)
	f, ctx, got := fetchedProxyAsset(t, want)
	require.Equal(t, d, got)
	c := f.Clients(harness.CacheProxy)
	data, err := c.Read(ctx, "", d)
	require.NoError(t, err)
	require.Equal(t, want, data)
	stream, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{RootDigest: d})
	require.NoError(t, err)
	rsp, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, rsp.Directories, 1)
}
