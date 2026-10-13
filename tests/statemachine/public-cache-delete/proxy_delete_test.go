package public_cache_delete_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

// Desired conformance: deleting remote CAS invalidates warmed proxy readers.
func TestWarmedProxyDelete(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	blob := []byte("audit warmed proxy deletion payload")
	d, err := app.Upload(ctx, "audit/nested", blob)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		b, err := proxy.Read(ctx, "audit/nested", d)
		require.NoError(t, err)
		require.Equal(t, blob, b)
	}
	req := wire(field(1, []byte(harness.ResourceName("audit/nested", d, false))))
	var res wire
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &req, &res, grpc.ForceCodec(codec{})))
	start := time.Now()
	var lastProxyErr error
	for _, after := range []time.Duration{0, 5 * time.Second, 30 * time.Second, 60 * time.Second} {
		if wait := after - time.Since(start); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		appBytes, appErr := app.Read(ctx, "audit/nested", d)
		require.Equal(t, codes.NotFound, status.Code(appErr))
		require.Empty(t, appBytes)
		proxyBytes, proxyErr := proxy.Read(ctx, "audit/nested", d)
		lastProxyErr = proxyErr
		if proxyErr != nil {
			require.Equal(t, codes.NotFound, status.Code(proxyErr))
		}
		if proxyErr == nil {
			require.Equal(t, blob, proxyBytes)
		}
		for name, client := range map[string]*harness.Clients{"app": app, "proxy": proxy} {
			missing, err := client.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "audit/nested", BlobDigests: []*repb.Digest{d}})
			require.NoError(t, err)
			require.Len(t, missing.MissingBlobDigests, 1)
			require.True(t, proto.Equal(d, missing.MissingBlobDigests[0]))
			batch, err := client.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "audit/nested", Digests: []*repb.Digest{d}})
			require.NoError(t, err)
			require.Len(t, batch.Responses, 1)
			require.True(t, proto.Equal(d, batch.Responses[0].Digest))
			expectedCode := int32(codes.NotFound)
			if name == "proxy" && proxyErr == nil {
				expectedCode = 0
			}
			require.Equal(t, expectedCode, batch.Responses[0].Status.Code)
			if batch.Responses[0].Status.Code == 0 {
				require.Equal(t, blob, batch.Responses[0].Data)
			}
			t.Logf("after=%s surface=%s missing=%d batch_status=%d", after, name, len(missing.MissingBlobDigests), batch.Responses[0].Status.Code)
		}
		t.Logf("after=%s app_read=%s proxy_read=%s proxy_bytes=%q", after, status.Code(appErr), status.Code(proxyErr), proxyBytes)
	}
	require.Equal(t, codes.NotFound, status.Code(lastProxyErr), "warmed proxy still serves app-deleted CAS after 60 seconds")
}
