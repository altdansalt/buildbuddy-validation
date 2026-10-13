package convergencecas

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Minimal cross-peer delete oracle: no content reads occur during the
// observation window until both peers report absence. Subsequent reads
// expose stale local bytes and cannot fetch deleted bytes from the app.
func TestDeleteInvalidatesHydratedProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	data := []byte("delete convergence isolated valid CAS payload")
	d, err := proxy.Upload(ctx, "", data)
	require.NoError(t, err)
	for _, c := range []*harness.Clients{app, proxy} {
		got, err := c.Read(ctx, "", d)
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
	uri := fmt.Sprintf("bytestream://app:1985/%s", harness.ResourceName("", d, false))
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", wrapperspb.String(uri), new(wrapperspb.BytesValue)))
	started := time.Now()
	stable := 0
	for time.Since(started) < 20*time.Second {
		matches := true
		for _, c := range []*harness.Clients{app, proxy} {
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			if len(missing.MissingBlobDigests) != 1 {
				matches = false
			}
		}

		// FindMissingBlobs on the proxy forwards to the app by default, whereas
		// ByteStream and batch reads may return locally cached bytes.
		if matches {
			got, err := proxy.Read(ctx, "", d)
			if status.Code(err) != codes.NotFound || len(got) != 0 {
				matches = false
			}
			batch, err := proxy.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}})
			require.NoError(t, err)
			require.Len(t, batch.Responses, 1)
			if batch.Responses[0].GetStatus().GetCode() != int32(codes.NotFound) {
				matches = false
			}
		}
		if matches {
			stable++
			if stable == 4 {
				t.Logf("delete convergence=%s", time.Since(started))
				return
			}
		} else {
			stable = 0
		}

		time.Sleep(100 * time.Millisecond)
	}
	for name, c := range map[string]*harness.Clients{"app": app, "proxy": proxy} {
		got, readErr := c.Read(ctx, "", d)
		batch, batchErr := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}})
		t.Logf("final peer=%s stream_code=%s stream_bytes=%d batch=%v batch_error=%v", name, status.Code(readErr), len(got), batch, batchErr)
	}
	// Finish the valid delete -> recreate transition before reporting the
	// isolated deletion mismatch. CAS recreation restores the same digest.
	recreated, err := app.Upload(ctx, "", data)
	require.NoError(t, err)
	require.Equal(t, d, recreated)
	for _, c := range []*harness.Clients{app, proxy} {
		got, err := c.Read(ctx, "", d)
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
	t.Fatalf("app delete did not converge to hydrated proxy within %s; presence + ByteStream + batch polling", time.Since(started))
}
