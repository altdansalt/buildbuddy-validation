package structures_test

import (
	"bytes"
	"context"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
	"time"
)

// A literal blob deletion is distinct from publishing an immutable root without
// a path. This isolated test requires the opt-in public cache-delete API.
func TestDeleteWarmedStructureBlob(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	content := []byte("structure-delete-recreate")
	d, err := proxy.Upload(ctx, instance, content)
	require.NoError(t, err)
	for _, c := range []*harness.Clients{app, proxy} {
		got, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		require.Equal(t, content, got)
	}
	var raw []byte
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", field(nil, 1, []byte("blobs/"+key(d))), &raw, grpc.ForceCodec(wireCodec{})))
	started := time.Now()
	var last string
	for time.Since(started) < 30*time.Second {
		converged := true
		for i, c := range []*harness.Clients{app, proxy} {
			got, err := c.Read(ctx, instance, d)
			missing, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
			absent := status.Code(err) == codes.NotFound && e == nil && len(missing.MissingBlobDigests) == 1
			if !absent {
				converged = false
				last = fmt.Sprintf("side=%d Read code=%s bytes=%d equals_original=%t missing=%v error=%v", i, status.Code(err), len(got), bytes.Equal(got, content), missing, e)
			}
		}
		if converged {
			t.Logf("deletion convergence %s", time.Since(started))
			_, err := app.Upload(ctx, instance, content)
			require.NoError(t, err)
			for _, c := range []*harness.Clients{app, proxy} {
				got, err := c.Read(ctx, instance, d)
				require.NoError(t, err)
				require.Equal(t, content, got)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("deletion failed to converge after %s: %s", time.Since(started), last)
}
