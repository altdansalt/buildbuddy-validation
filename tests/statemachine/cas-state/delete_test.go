package casstate_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	sp "github.com/altdansalt/buildbuddy-validation/tests/statemachine/cas-state/surfaces"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Minimal reproduction: valid proxy write, warm read, successful app delete.
// The proxy's FindMissingBlobs consults the app while its Read serves local bytes.
func TestDeleteRecreateState(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	instance := "cas-delete"
	data := []byte("delete and recreate this immutable CAS payload")
	d := harness.Digest(data)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	r, err := proxy.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: data}}, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Len(t, r.Responses, 1)
	require.Zero(t, r.Responses[0].GetStatus().GetCode())
	for _, c := range []*harness.Clients{app, proxy} {
		got, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &sp.FileRequest{Uri: harness.ResourceName(instance, d, false)}, &sp.Empty{}))
	started := time.Now()
	deadline := started.Add(5 * time.Second)
	for {
		for _, c := range []*harness.Clients{app, proxy} {
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, missing.MissingBlobDigests, 1)
			require.Equal(t, d.Hash, missing.MissingBlobDigests[0].Hash)
			require.Equal(t, d.SizeBytes, missing.MissingBlobDigests[0].SizeBytes)
		}
		_, err := app.Read(ctx, instance, d)
		require.Equal(t, codes.NotFound, status.Code(err), "app must expose deletion")
		got, proxyErr := proxy.Read(ctx, instance, d)
		if proxyErr != nil {
			require.Equal(t, codes.NotFound, status.Code(proxyErr))
			t.Logf("read convergence after %s", time.Since(started))
			for i := 0; i < 2; i++ {
				time.Sleep(100 * time.Millisecond)
				_, err := proxy.Read(ctx, instance, d)
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			return
		}
		require.Equal(t, data, got)
		if time.Now().After(deadline) {
			t.Fatalf("divergence persisted %s: app Read missing; app and proxy FindMissingBlobs missing; proxy Read returns exact deleted bytes %q", time.Since(started), got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
