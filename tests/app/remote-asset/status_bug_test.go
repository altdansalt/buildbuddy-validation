package remoteasset_test

import (
	"net/http"
	"testing"
	"time"

	asset "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestOriginErrorStatus(t *testing.T) {
	_, fetch, origin, ctx := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
			}
		case "/forbidden":
			w.WriteHeader(403)
			return
		}
		w.Write([]byte("actual content"))
	}))
	for _, tc := range []struct {
		name, path string
		code       codes.Code
		q          []*asset.Qualifier
		timeout    time.Duration
	}{
		{"origin timeout", "/slow", codes.DeadlineExceeded, nil, 100 * time.Millisecond},
		{"origin denies access", "/forbidden", codes.PermissionDenied, nil, 5 * time.Second},
		{"checksum mismatch", "/blob", codes.Aborted, []*asset.Qualifier{sri("expected content")}, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{origin + tc.path}, Qualifiers: tc.q, Timeout: durationpb.New(tc.timeout)})
			require.NoError(t, err)
			t.Logf("inline status: %v", resp.GetStatus())
			require.EqualValues(t, tc.code, resp.GetStatus().GetCode())
		})
	}
}
