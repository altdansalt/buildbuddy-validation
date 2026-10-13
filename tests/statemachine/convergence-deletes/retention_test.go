package convergence_test

import (
	"context"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"testing"
	"time"
)

// This checks the requested agreement invariant, not an advertised invalidation
// guarantee. Independent proxy storage has no app-delete invalidation in v2.314.0.
func TestDeletedCASReplicaAgreement(t *testing.T) {
	for attempt := 0; attempt < 2; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
			data := []byte("immutable replica consistency")
			d, err := app.Upload(ctx, "retention", data)
			require.NoError(t, err)
			got, err := proxy.Read(ctx, "retention", d)
			require.NoError(t, err)
			require.Equal(t, data, got)
			require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", wrapperspb.String(harness.ResourceName("retention", d, false)), &emptypb.Empty{}))
			started := time.Now()
			var lastErr error
			for {
				_, err = app.Read(ctx, "retention", d)
				require.Equal(t, codes.NotFound, status.Code(err))
				missing, err := proxy.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "retention", BlobDigests: []*repb.Digest{d}})
				require.NoError(t, err)
				require.Len(t, missing.MissingBlobDigests, 1)
				require.Equal(t, d.Hash, missing.MissingBlobDigests[0].Hash)
				got, lastErr = proxy.Read(ctx, "retention", d)
				if status.Code(lastErr) == codes.NotFound {
					break
				}
				require.NoError(t, lastErr)
				require.Equal(t, data, got)
				if time.Since(started) >= 5*time.Second {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			t.Logf("proxy CAS agreement observation duration=%s read_status=%s", time.Since(started), status.Code(lastErr))
			require.Equal(t, codes.NotFound, status.Code(lastErr), "proxy FindMissing says absent but ByteStream still serves deleted bytes")
		})
	}
}
