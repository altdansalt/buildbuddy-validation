package remoteasset_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	asset "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestOldestContentAccepted(t *testing.T) {
	var hits atomic.Int32
	_, fetch, origin, ctx := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte("freshness content")) }))
	req := &asset.FetchBlobRequest{Uris: []string{origin + "/blob"}, Qualifiers: []*asset.Qualifier{sri("freshness content")}}
	resp, err := fetch.FetchBlob(ctx, req)
	require.NoError(t, err)
	require.EqualValues(t, codes.OK, resp.GetStatus().GetCode())
	require.EqualValues(t, 1, hits.Load())
	// Ensure the lower bound postdates the completed original retrieval.
	time.Sleep(20 * time.Millisecond)
	req.OldestContentAccepted = timestamppb.Now()
	resp, err = fetch.FetchBlob(ctx, req)
	require.NoError(t, err)
	require.EqualValues(t, codes.OK, resp.GetStatus().GetCode())
	t.Logf("origin requests after freshness-constrained fetch: %d", hits.Load())
	require.EqualValues(t, 2, hits.Load(), "content retrieved before oldest_content_accepted must be fetched again")
}
