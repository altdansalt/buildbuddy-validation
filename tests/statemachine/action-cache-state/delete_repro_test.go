package actioncachestate_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// Public deletion is opt-in. A warmed proxy should converge to the authoritative
// app's absence. This isolated test intentionally fails if stale bytes persist.
func TestCASDeleteConvergesAtProxy(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	instance := "delete/shared"
	want := []byte("cached before authoritative deletion")
	d, err := proxy.Write(ctx, instance, want, false)
	require.NoError(t, err)
	for _, reader := range []*harness.Clients{app, proxy} {
		b, err := reader.Read(ctx, instance, d)
		require.NoError(t, err)
		require.Equal(t, want, b)
	}
	uri := harness.ResourceName(instance, d, false)
	req := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte(uri))
	var res []byte
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &req, &res, grpc.ForceCodec(deleteCodec{})))
	_, err = app.Read(ctx, instance, d)
	require.Equal(t, codes.NotFound, status.Code(err))
	missing, err := app.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Len(t, missing.MissingBlobDigests, 1)
	start := time.Now()
	deadline := start.Add(10 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		last, err = proxy.Read(ctx, instance, d)
		if status.Code(err) == codes.NotFound {
			for i := 0; i < 3; i++ {
				time.Sleep(100 * time.Millisecond)
				_, err = proxy.Read(ctx, instance, d)
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			t.Logf("delete converged after %s", time.Since(start))
			return
		}
		require.NoError(t, err)
		require.Equal(t, want, last)
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("app absent, proxy retains original bytes after %s: %q", time.Since(start), last)
	require.Equal(t, codes.NotFound, status.Code(err), "proxy must converge after authoritative CAS deletion")
}

type deleteCodec struct{}

func (deleteCodec) Name() string                  { return "proto" }
func (deleteCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (deleteCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
