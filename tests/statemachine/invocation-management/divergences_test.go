package invocation_management_test

import (
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDeletedDownloadIsNotFound(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	id := upload(t, f, []string{"deleted-download\n"}, 0)
	get := func() (int, string) {
		r, e := http.NewRequest("GET", f.Endpoint(harness.App).HTTP+"/file/download?"+url.Values{"invocation_id": {id}, "artifact": {"raw_json"}}.Encode(), nil)
		require.NoError(t, e)
		r.Header.Set("x-buildbuddy-api-key", f.APIKey)
		resp, e := (&http.Client{Timeout: 15 * time.Second}).Do(r)
		require.NoError(t, e)
		defer resp.Body.Close()
		b, e := io.ReadAll(resp.Body)
		require.NoError(t, e)
		return resp.StatusCode, string(b)
	}
	code, _ := get()
	require.Equal(t, 200, code)
	_, e := call(f.Clients(harness.App), ui+"DeleteInvocation", str(2, id))
	require.NoError(t, e)
	_, e = call(f.Clients(harness.App), ui+"GetInvocation", field(2, str(1, id)))
	require.Equal(t, codes.NotFound, status.Code(e))
	start := time.Now()
	// Observe the mismatch for a bounded window before asserting the contract.
	for time.Since(start) < 3*time.Second {
		code, body := get()
		t.Logf("deleted download elapsed=%s status=%d body=%s", time.Since(start), code, body)
		if code == 404 {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	code, body := get()
	require.Equal(t, 404, code, body)
}

func TestDeletedOwnerIsNotFound(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	configureOwnerRead(t, f)
	owner := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "admin", []string{"ORG_ADMIN"}))
	id := upload(t, f, []string{"owner deletion\n"}, 0)
	b, err := call(owner, ui+"GetInvocationOwner", str(2, id))
	require.NoError(t, err)
	require.Equal(t, f.GroupID, string(first(t, b, 2)))
	_, err = call(f.Clients(harness.App), ui+"DeleteInvocation", str(2, id))
	require.NoError(t, err)
	start := time.Now()
	for time.Since(start) < 3*time.Second {
		_, err = call(owner, ui+"GetInvocationOwner", str(2, id))
		t.Logf("deleted owner elapsed=%s err=%v", time.Since(start), err)
		if status.Code(err) == codes.NotFound {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	require.Equal(t, codes.NotFound, status.Code(err))
}
