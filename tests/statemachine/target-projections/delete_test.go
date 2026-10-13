package projections_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// DeleteFile has an explicit feature gate. Warm the proxy before deleting at
// the authoritative app, then require the same absence at both read surfaces.
func TestDeleteFileInvalidatesWarmProxy(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	data := []byte("target-projections deletion consistency")
	d, err := f.Clients(harness.CacheProxy).Upload(ctx, "", data)
	require.NoError(t, err)
	for _, c := range []harness.Component{harness.App, harness.CacheProxy} {
		b, err := f.Clients(c).Read(ctx, "", d)
		require.NoError(t, err)
		require.Equal(t, data, b)
	}
	deleteViaApp(t, ctx, f, harness.ResourceName("", d, false))
	missing, err := f.Clients(harness.App).CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Len(t, missing.MissingBlobDigests, 1)
	require.Equal(t, d.Hash, missing.MissingBlobDigests[0].Hash)
	require.Equal(t, d.SizeBytes, missing.MissingBlobDigests[0].SizeBytes)
	started := time.Now()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		t.Logf("deletion observation window=%s; app=%s proxy=%s", time.Since(started), casState(ctx, f, f.Clients(harness.App), d, "", false), casState(ctx, f, f.Clients(harness.CacheProxy), d, "", false))
	}()
	require.Eventually(t, func() bool {
		missing, err := f.Clients(harness.CacheProxy).CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
		if err != nil || len(missing.MissingBlobDigests) != 1 {
			return false
		}
		_, err = f.Clients(harness.CacheProxy).Read(ctx, "", d)
		return err != nil
	}, 60*time.Second, 250*time.Millisecond, "app deletion did not invalidate warmed proxy within 60 seconds")
}
