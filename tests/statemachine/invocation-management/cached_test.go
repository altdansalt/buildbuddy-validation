package invocation_management_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
	"time"
)

func TestAPICachedTargetDeleted(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--api.enable_cache=true"}}})
	id := upload(t, f, []string{"cached target\n"}, 0)
	req := field(1, msg(str(1, id), str(4, "//sample:test")))
	b, e := call(f.Clients(harness.App), "/api.v1.ApiService/GetTarget", req)
	require.NoError(t, e)
	require.NotEmpty(t, bytesFields(t, b, 1))
	_, e = call(f.Clients(harness.App), ui+"DeleteInvocation", str(2, id))
	require.NoError(t, e)
	_, e = call(f.Clients(harness.App), ui+"GetInvocation", field(2, str(1, id)))
	require.Equal(t, codes.NotFound, status.Code(e))
	start := time.Now()
	for time.Since(start) < 3*time.Second {
		b, e = call(f.Clients(harness.App), "/api.v1.ApiService/GetTarget", req)
		t.Logf("deleted cached target elapsed=%s code=%s targets=%d", time.Since(start), status.Code(e), len(bytesFields(t, b, 1)))
		if status.Code(e) == codes.NotFound {
			return
		}
		require.NoError(t, e)
		time.Sleep(300 * time.Millisecond)
	}
	require.Equal(t, codes.NotFound, status.Code(e))
}
