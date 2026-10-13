package usagealerts

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/grpcreflect"
	"github.com/stretchr/testify/require"
)

func TestRuleLifecycle(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--app.region=fixture", "--app.usage_tracking_enabled=true", "--app.usage_alerts_enabled=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// Use disposable organization-admin keys for gRPC and cookie auth for HTTP.
	groups := []string{f.GroupID, f.Web.CreateGroup(t, "alert-isolation")}
	connections := map[string]*harness.Clients{}
	for _, group := range groups {
		key := f.Web.CreateAPIKey(t, group, "alert-admin", []string{"ORG_ADMIN"})
		connections[group] = harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
	}
	c := connections[groups[0]]
	rc := grpcreflect.NewClientAuto(ctx, c.Conn)
	defer rc.Reset()
	svc, err := rc.ResolveService("buildbuddy.service.BuildBuddyService")
	require.NoError(t, err)
	grpcCall := func(method string, req map[string]any) map[string]any {
		t.Helper()
		md := svc.FindMethodByName(method)
		require.NotNil(t, md)
		in, out := dynamic.NewMessage(md.GetInputType()), dynamic.NewMessage(md.GetOutputType())
		b, err := json.Marshal(req)
		require.NoError(t, err)
		require.NoError(t, in.UnmarshalJSON(b))
		require.NoError(t, connections[req["requestContext"].(map[string]any)["groupId"].(string)].Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, in, out))
		b, err = out.MarshalJSON()
		require.NoError(t, err)
		var result map[string]any
		require.NoError(t, json.Unmarshal(b, &result))
		return result
	}
	models := map[string]map[string]any{groups[0]: {}, groups[1]: {}}
	webCall := func(method string, req map[string]any) map[string]any {
		t.Helper()
		r, err := f.Web.RPC(method, req)
		require.NoError(t, err)
		return r
	}
	request := func(group string) map[string]any {
		return map[string]any{"requestContext": f.Web.RequestContext(group)}
	}
	// Compare sets, not database order. Retain the complete create response in
	// the model so creator/timestamp/configuration cannot drift across surfaces.
	check := func() {
		t.Helper()
		for _, group := range groups {
			for _, call := range []func(string, map[string]any) map[string]any{grpcCall, webCall} {
				deadline := time.Now().Add(10 * time.Second)
				ticker := time.NewTicker(100 * time.Millisecond)
				for {
					result := call("GetUsageAlertingRules", request(group))
					actual := map[string]any{}
					if rules, ok := result["usageAlertingRule"].([]any); ok {
						for _, r := range rules {
							rule := r.(map[string]any)
							id := rule["metadata"].(map[string]any)["usageAlertingRuleId"].(string)
							actual[id] = rule
						}
					}
					if reflect.DeepEqual(models[group], actual) {
						break
					}
					if time.Now().After(deadline) {
						ticker.Stop()
						require.Equal(t, models[group], actual)
						return
					}
					select {
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-ticker.C:
					}
				}
				ticker.Stop()
			}
			// The usage-page context must agree across transports after each step.
			require.Equal(t, grpcCall("GetUsage", request(group)), webCall("GetUsage", request(group)))
		}
	}
	check()
	create := func(group string, metric, window int, threshold string) string {
		req := request(group)
		config := map[string]any{"metric": metric, "window": window, "absoluteThreshold": threshold}
		req["configuration"] = config
		result := grpcCall("CreateUsageAlertingRule", req)
		rule := result["usageAlertingRule"].(map[string]any)
		id := rule["metadata"].(map[string]any)["usageAlertingRuleId"].(string)
		require.NotEmpty(t, id)
		require.NotContains(t, models[group], id)
		// Assert the requested configuration independently of the read oracle.
		cfg := rule["configuration"].(map[string]any)
		require.Equal(t, threshold, fmt.Sprint(cfg["absoluteThreshold"]))
		require.Equal(t, map[int]string{1: "INVOCATIONS", 9: "TOTAL_UPLOAD_SIZE_BYTES", 4: "CAS_CACHE_HITS"}[metric], cfg["metric"])
		require.Equal(t, map[int]string{1: "DAY", 2: "WEEK", 3: "MONTH"}[window], cfg["window"])
		models[group][id] = rule
		check()
		return id
	}
	a := create(groups[0], 1, 1, "1000000")
	b := create(groups[0], 9, 2, "2000000")
	other := create(groups[1], 4, 3, "3000000")
	// Configuration uniqueness is scoped to the organization.
	repeated := create(groups[1], 1, 1, "1000000")
	for _, item := range []struct{ group, id string }{{groups[0], a}, {groups[1], repeated}, {groups[1], other}, {groups[0], b}} {
		req := request(item.group)
		req["usageAlertingRuleId"] = item.id
		grpcCall("DeleteUsageAlertingRule", req)
		delete(models[item.group], item.id)
		check()
	}
	// Re-create a deleted configuration; its identity must be fresh.
	fresh := create(groups[0], 1, 1, "1000000")
	require.NotEqual(t, a, fresh)
	req := request(groups[0])
	req["usageAlertingRuleId"] = fresh
	grpcCall("DeleteUsageAlertingRule", req)
	delete(models[groups[0]], fresh)
	check()
}
