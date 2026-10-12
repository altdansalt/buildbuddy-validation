package invocations_api_test

import (
	"encoding/base64"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUploadedInvocationConsistency(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ids := []string{upload(t, f, []string{"hello stdout\n", "second line: λ\n"}, 0), upload(t, f, []string{"failed build\n"}, 1)}
	for i, id := range ids {
		t.Run(id, func(t *testing.T) {
			res := rpc(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})
			invs := objects(t, res, "invocation")
			require.Len(t, invs, 1)
			inv := invs[0].(map[string]any)
			require.Equal(t, id, inv["invocationId"])
			require.Equal(t, "test", inv["command"])
			require.Equal(t, "fixture-host", inv["host"])
			require.Equal(t, "fixture-user", inv["user"])
			require.Equal(t, i == 0, inv["success"] == true)
			require.Equal(t, "COMPLETE_INVOCATION_STATUS", inv["invocationStatus"])
			require.Equal(t, "1500000", inv["durationUsec"])
			require.Equal(t, "0123456789abcdef", inv["commitSha"])
			code, v1 := api(t, f, "GetInvocation", map[string]any{"selector": map[string]any{"invocationId": id}, "includeMetadata": true})
			require.Equal(t, 200, code)
			vs := objects(t, v1, "invocation")
			require.Len(t, vs, 1)
			v := vs[0].(map[string]any)
			require.Equal(t, inv["command"], v["command"])
			require.Equal(t, inv["durationUsec"], v["durationUsec"])
			require.NotEmpty(t, v["buildMetadata"])
		})
	}
	t.Run("logs", func(t *testing.T) {
		res := rpc(t, f, "GetEventLogChunk", map[string]any{"invocationId": ids[0], "chunkId": "0000"})
		b, err := base64.StdEncoding.DecodeString(res["buffer"].(string))
		require.NoError(t, err)
		require.Equal(t, "hello stdout\nsecond line: λ\n", string(b))
		code, res := api(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": ids[0]}})
		require.Equal(t, 200, code)
		require.Equal(t, string(b), res["log"].(map[string]any)["contents"])
	})
	t.Run("targets", func(t *testing.T) {
		code, res := api(t, f, "GetTarget", map[string]any{"selector": map[string]any{"invocationId": ids[0], "label": "//sample:test"}})
		require.Equal(t, 200, code)
		targets := objects(t, res, "target")
		require.Len(t, targets, 1)
		target := targets[0].(map[string]any)
		require.Equal(t, "//sample:test", target["label"])
		require.Equal(t, "PASSED", target["status"])
		res = rpc(t, f, "GetTarget", map[string]any{"invocationId": ids[0], "targetLabel": "//sample:test"})
		groups := objects(t, res, "targetGroups")
		require.NotEmpty(t, groups)
		found := false
		for _, g := range groups {
			for _, v := range objects(t, g.(map[string]any), "targets") {
				target := v.(map[string]any)
				if len(objects(t, target, "testResultEvents")) > 0 {
					found = true
					require.Equal(t, "PASSED", target["testSummary"].(map[string]any)["overallStatus"])
				}
			}
		}
		require.True(t, found, "test results must survive BES storage")
	})
	t.Run("search", func(t *testing.T) {
		res := rpc(t, f, "SearchInvocation", map[string]any{"query": map[string]any{"groupId": f.GroupID, "user": "fixture-user", "host": "fixture-host"}, "count": 1})
		seen := map[string]bool{}
		for n := 0; n < 4; n++ {
			for _, v := range objects(t, res, "invocation") {
				id := v.(map[string]any)["invocationId"].(string)
				require.False(t, seen[id])
				seen[id] = true
			}
			token, _ := res["nextPageToken"].(string)
			if token == "" {
				break
			}
			res = rpc(t, f, "SearchInvocation", map[string]any{"query": map[string]any{"groupId": f.GroupID, "user": "fixture-user", "host": "fixture-host"}, "count": 1, "pageToken": token})
		}
		require.Len(t, seen, 2)
		for _, id := range ids {
			require.True(t, seen[id])
		}
		res = rpc(t, f, "SearchInvocation", map[string]any{"query": map[string]any{"groupId": f.GroupID, "user": "fixture-user", "host": "other-host"}})
		require.Empty(t, objects(t, res, "invocation"))
	})
	t.Run("invalid selectors", func(t *testing.T) {
		for _, method := range []string{"GetInvocation", "GetTarget", "GetLog"} {
			code, _ := api(t, f, method, map[string]any{})
			require.Equal(t, 400, code, method)
		}
		_, err := f.Web.RPC("GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": uuid.NewString()}})
		require.ErrorContains(t, err, "HTTP 404")
	})
}
