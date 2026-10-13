package invocation_management_test

import (
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestOLAPDeleteConverges(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--app.enable_target_tracking=true", "--app.enable_write_test_target_statuses_to_olap_db=true", "--app.enable_read_target_statuses_from_olap_db=true"}}})
	configureOwnerRead(t, f, true)
	id := upload(t, f, []string{"OLAP conformance\n"}, 0)
	trend := func() uint64 {
		b, e := call(f.Clients(harness.App), ui+"GetTrend", msg(field(1, str(2, f.GroupID)), field(2, str(1, "fixture-user"))))
		require.NoError(t, e)
		var total uint64
		for _, r := range bytesFields(t, b, 2) {
			total += varintField(t, r, 3)
		}
		return total
	}
	inspect := func(stage string) {
		for _, method := range []string{"GetTrend", "GetInvocationStat", "GetTargetHistory", "GetInvocationFilterSuggestions"} {
			req := map[string]any{}
			switch method {
			case "GetTrend", "GetInvocationStat":
				req["query"] = map[string]any{"user": "fixture-user"}
			case "GetTargetHistory":
				req["query"] = map[string]any{"repoUrl": "https://example.invalid/conformance"}
			}
			if method == "GetInvocationStat" {
				req["aggregationType"] = "USER_AGGREGATION_TYPE"
			}
			j := rpc(t, f, method, req)
			t.Logf("OLAP %s %s HTTP=%v", stage, method, j)
		}
	}
	start := time.Now()
	for trend() != 1 {
		require.Less(t, time.Since(start), 15*time.Second, "initial OLAP ingestion")
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("OLAP initial convergence within %s", time.Since(start))
	inspect("created")
	j := rpc(t, f, "GetInvocationFilterSuggestions", map[string]any{})
	require.Contains(t, fmt.Sprint(j["suggestions"]), "fixture-user")
	_, e := call(f.Clients(harness.App), ui+"DeleteInvocation", str(2, id))
	require.NoError(t, e)
	inspect("deleted")
	start = time.Now()
	for time.Since(start) < 5*time.Second {
		n := trend()
		t.Logf("deleted OLAP trend elapsed=%s total=%d", time.Since(start), n)
		if n == 0 {
			time.Sleep(200 * time.Millisecond)
			require.Zero(t, trend())
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	require.Zero(t, trend(), "OLAP retains deleted invocation")
}
