package public_cache_delete_test

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

// rawCodec sends a pre-encoded api.v1.DeleteFileRequest without generated bindings.
type rawCodec struct{}

func (rawCodec) Name() string                  { return "proto" }
func (rawCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (rawCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}

// After the app deletes a CAS blob, a proxy that already cached it reports the
// blob missing (FindMissingBlobs passes through to the app) yet still serves
// its bytes over ByteStream and BatchReadBlobs. The app is the control.
func TestWarmedProxyServesDeletedBlob(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.CacheProxy},
		Args:       map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}},
	})
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const inst = "delete"
	blob := []byte("warmed proxy deletion payload")
	d, err := app.Upload(ctx, inst, blob)
	require.NoError(t, err)
	for i := 0; i < 2; i++ { // warm the proxy's local cache
		got, err := proxy.Read(ctx, inst, d)
		require.NoError(t, err)
		require.Equal(t, blob, got)
	}

	name := harness.ResourceName(inst, d, false)
	req := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte(name))
	var res []byte
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &req, &res, grpc.ForceCodec(rawCodec{})))

	// Control: the app agrees the blob is gone, on every surface.
	_, err = app.Read(ctx, inst, d)
	require.Equal(t, codes.NotFound, status.Code(err))
	for name, c := range map[string]*harness.Clients{"app": app, "proxy": proxy} {
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: inst, BlobDigests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, missing.MissingBlobDigests, 1, "%s FindMissingBlobs", name)
	}

	// Divergence: the proxy must stop serving what it reports missing. Sample
	// at 0, 5, 30 and 60s; the observed behavior never converges.
	start := time.Now()
	for _, after := range []time.Duration{0, 5 * time.Second, 30 * time.Second, 60 * time.Second} {
		time.Sleep(time.Until(start.Add(after)))
		_, err := proxy.Read(ctx, inst, d)
		batch, berr := proxy.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: inst, Digests: []*repb.Digest{d}})
		require.NoError(t, berr)
		require.Len(t, batch.Responses, 1)
		t.Logf("after=%s proxy_bytestream=%s proxy_batch=%d", after, status.Code(err), batch.Responses[0].Status.Code)
		if after == 60*time.Second {
			require.Equal(t, codes.NotFound, status.Code(err), "proxy ByteStream still serves deleted blob it reports missing")
			require.Equal(t, int32(codes.NotFound), batch.Responses[0].Status.Code, "proxy BatchReadBlobs still serves deleted blob")
		}
	}
}
