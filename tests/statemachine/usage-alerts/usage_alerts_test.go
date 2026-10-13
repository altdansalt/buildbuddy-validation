package usagealerts

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/grpcreflect"
	"github.com/stretchr/testify/require"
)

func TestRuleLifecycle(t *testing.T) {
	// The runner must provide its Docker daemon (init-dockerd=true).
	docker := func(args ...string) string {
		t.Helper()
		b, err := exec.Command("docker", args...).CombinedOutput()
		require.NoError(t, err, "%s", b)
		return strings.TrimSpace(string(b))
	}
	name := fmt.Sprintf("usage-olap-%d", time.Now().UnixNano())
	docker("run", "-d", "--name", name, "-e", "CLICKHOUSE_SKIP_USER_SETUP=1", "-p", "0.0.0.0::9000", "mirror.gcr.io/clickhouse/clickhouse-server@sha256:1ffa82edee000a42c09313bd9f1293d94c570aee74babc1b3ca9983a35fa597b")
	t.Cleanup(func() {
		b, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); dir != "" {
			_ = os.WriteFile(filepath.Join(dir, "usage-clickhouse.log"), b, 0644)
		}
		docker("rm", "-fv", name)
	})
	port := strings.TrimPrefix(docker("port", name, "9000/tcp"), "0.0.0.0:")
	port = strings.Split(port, "\n")[0]
	require.Eventually(t, func() bool {
		return exec.Command("docker", "exec", name, "clickhouse-client", "--query", "SELECT 1").Run() == nil
	}, time.Minute, 200*time.Millisecond)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, DockerArgs: map[harness.Component][]string{harness.App: {"--add-host=host.docker.internal:host-gateway"}}, Args: map[harness.Component][]string{harness.App: {"--app.region=fixture", "--app.usage_tracking_enabled=true", "--app.usage_alerts_enabled=true", "--app.audit_logs_enabled=true", "--olap_database.data_source=clickhouse://default:@host.docker.internal:" + port + "/default"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// Use disposable organization-admin keys for gRPC and cookie auth for HTTP.
	suiteAt := time.Now()
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
	second := map[string]*harness.Clients{}
	for _, group := range groups {
		second[group] = harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, group, "alert-peer", []string{"ORG_ADMIN"}))
	}
	primary := connections
	peerCall := func(method string, req map[string]any) map[string]any {
		connections = second
		defer func() { connections = primary }()
		return grpcCall(method, req)
	}
	auditModel := map[string][]any{groups[0]: {}, groups[1]: {}}
	models := map[string]map[string]any{groups[0]: {}, groups[1]: {}}
	webCall := func(method string, req map[string]any) map[string]any {
		t.Helper()
		r, err := f.Web.RPC(method, req)
		require.NoError(t, err)
		return r
	}
	// Audit addition: frontend uses binary protobuf, not harness JSON HTTP.
	binaryWebCall := func(method string, req map[string]any) map[string]any {
		t.Helper()
		md := svc.FindMethodByName(method)
		require.NotNil(t, md)
		in, out := dynamic.NewMessage(md.GetInputType()), dynamic.NewMessage(md.GetOutputType())
		b, err := json.Marshal(req)
		require.NoError(t, err)
		require.NoError(t, in.UnmarshalJSON(b))
		b, err = in.Marshal()
		require.NoError(t, err)
		hreq, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(b))
		require.NoError(t, err)
		hreq.Header.Set("Content-Type", "application/proto")
		resp, err := f.Web.Client.Do(hreq)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "binary HTTP %s: %s", method, b)
		require.NoError(t, out.Unmarshal(b))
		b, err = out.MarshalJSON()
		require.NoError(t, err)
		var result map[string]any
		require.NoError(t, json.Unmarshal(b, &result))
		return result
	}
	prefixedWebCall := func(method string, req map[string]any) map[string]any {
		t.Helper()
		md := svc.FindMethodByName(method)
		require.NotNil(t, md)
		in, out := dynamic.NewMessage(md.GetInputType()), dynamic.NewMessage(md.GetOutputType())
		b, err := json.Marshal(req)
		require.NoError(t, err)
		require.NoError(t, in.UnmarshalJSON(b))
		b, err = in.Marshal()
		require.NoError(t, err)
		frame := make([]byte, 5+len(b))
		binary.BigEndian.PutUint32(frame[1:5], uint32(len(b)))
		copy(frame[5:], b)
		b = frame
		hreq, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(b))
		require.NoError(t, err)
		hreq.Header.Set("Content-Type", "application/proto+prefixed")
		resp, err := f.Web.Client.Do(hreq)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "binary HTTP %s: %s", method, b)
		var payload []byte
		messages, trailers := 0, 0
		for len(b) > 0 {
			require.GreaterOrEqual(t, len(b), 5)
			flags, n := b[0], int(binary.BigEndian.Uint32(b[1:5]))
			require.GreaterOrEqual(t, len(b), 5+n)
			body := b[5 : 5+n]
			if flags == 0x80 {
				trailers++
				require.Contains(t, strings.ToLower(string(body)), "grpc-status: 0")
			} else {
				require.Equal(t, byte(0), flags)
				messages++
				payload = body
			}
			b = b[5+n:]
		}
		require.Equal(t, 1, messages)
		require.Equal(t, 1, trailers)
		require.NoError(t, out.Unmarshal(payload))
		b, err = out.MarshalJSON()
		require.NoError(t, err)
		var result map[string]any
		require.NoError(t, json.Unmarshal(b, &result))
		return result
	}
	request := func(group string) map[string]any {
		return map[string]any{"requestContext": f.Web.RequestContext(group)}
	}
	// Compare sets, not database order. Retain the complete create response in
	// the model so creator/timestamp/configuration cannot drift across surfaces.
	mutationAt := time.Now()
	check := func() {
		t.Helper()
		for _, group := range groups {
			for surface, call := range []func(string, map[string]any) map[string]any{grpcCall, peerCall, webCall, binaryWebCall, prefixedWebCall} {
				started := time.Now()
				deadline := started.Add(10 * time.Second)
				stable := 0
				ticker := time.NewTicker(100 * time.Millisecond)
				for {
					result := call("GetUsageAlertingRules", request(group))
					actual := map[string]any{}
					raw, present := result["usageAlertingRule"]
					if present {
						_, ok := raw.([]any)
						require.True(t, ok, "rules must be an array")
					}
					// Protobuf JSON legitimately omits an empty repeated field.
					if rules, ok := raw.([]any); ok {
						for _, r := range rules {
							rule := r.(map[string]any)
							id := rule["metadata"].(map[string]any)["usageAlertingRuleId"].(string)
							require.NotContains(t, actual, id, "duplicate rule ID")
							actual[id] = rule
						}
					}
					if reflect.DeepEqual(models[group], actual) {
						stable++
						if stable == 1 {
							t.Logf("rule convergence group=%s surface=%d read=%s since-mutation=%s", group, surface, time.Since(started), time.Since(mutationAt))
						}
						if stable == 3 {
							break
						}
					} else {
						stable = 0
					}
					if time.Now().After(deadline) {
						ticker.Stop()
						require.Equal(t, models[group], actual)
						t.Fatal("rule stability deadline exceeded")
					}
					select {
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-ticker.C:
					}
				}
				ticker.Stop()
			}
			var canonicalEntries []any
			for surface, call := range []func(string, map[string]any) map[string]any{grpcCall, peerCall, webCall, binaryWebCall, prefixedWebCall} {
				started := time.Now()
				stable := 0
				for {
					var actual []any
					var entriesForRules []any
					req := request(group)
					req["timestampAfter"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
					req["timestampBefore"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
					for {
						result := call("GetAuditLogs", req)
						if entries, ok := result["entries"].([]any); ok {
							for _, raw := range entries {
								entry := raw.(map[string]any)
								payload := entry["request"].(map[string]any)["apiRequest"].(map[string]any)
								if _, ok := payload["createUsageAlertingRule"]; ok {
									require.Equal(t, "CREATE", entry["action"])
									require.Equal(t, "alert-peer", entry["authenticationInfo"].(map[string]any)["apiKey"].(map[string]any)["label"])
									actual = append(actual, payload)
									entriesForRules = append(entriesForRules, entry)
									require.Equal(t, "GROUP", entry["resource"].(map[string]any)["type"])
									auth := entry["authenticationInfo"].(map[string]any)["apiKey"].(map[string]any)
									require.NotEmpty(t, auth["id"])
									eventTime, err := time.Parse(time.RFC3339Nano, entry["eventTime"].(string))
									require.NoError(t, err)
									require.True(t, !eventTime.Before(suiteAt.Add(-time.Second)) && !eventTime.After(time.Now().Add(time.Second)))
								}
								if _, ok := payload["deleteUsageAlertingRule"]; ok {
									require.Equal(t, "DELETE", entry["action"])
									require.Equal(t, "alert-admin", entry["authenticationInfo"].(map[string]any)["apiKey"].(map[string]any)["label"])
									actual = append(actual, payload)
									entriesForRules = append(entriesForRules, entry)
									require.Equal(t, "GROUP", entry["resource"].(map[string]any)["type"])
									auth := entry["authenticationInfo"].(map[string]any)["apiKey"].(map[string]any)
									require.NotEmpty(t, auth["id"])
									eventTime, err := time.Parse(time.RFC3339Nano, entry["eventTime"].(string))
									require.NoError(t, err)
									require.True(t, !eventTime.Before(suiteAt.Add(-time.Second)) && !eventTime.After(time.Now().Add(time.Second)))
								}
							}
						}
						token, _ := result["nextPageToken"].(string)
						if token == "" {
							break
						}
						req["pageToken"] = token
					}
					if len(actual) == len(auditModel[group]) {
						require.ElementsMatch(t, auditModel[group], actual)
						stable++
						if stable == 1 {
							t.Logf("audit convergence group=%s surface=%d read=%s since-mutation=%s", group, surface, time.Since(started), time.Since(mutationAt))
						}
						if stable == 3 {
							if surface == 0 {
								canonicalEntries = entriesForRules
							} else {
								require.ElementsMatch(t, canonicalEntries, entriesForRules)
							}
							break
						}
					} else {
						stable = 0
					}
					if time.Since(started) > 10*time.Second {
						require.ElementsMatch(t, auditModel[group], actual)
						t.Fatal("audit convergence timeout")
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
			// The usage-page context must agree across transports after each step.
			require.Equal(t, grpcCall("GetUsage", request(group)), webCall("GetUsage", request(group)))
			require.Equal(t, grpcCall("GetUsage", request(group)), binaryWebCall("GetUsage", request(group)))
			require.Equal(t, grpcCall("GetUsage", request(group)), prefixedWebCall("GetUsage", request(group)))
		}
	}
	check()
	create := func(group string, metric, window int, threshold string) string {
		req := request(group)
		config := map[string]any{"metric": metric, "window": window, "absoluteThreshold": threshold}
		req["configuration"] = config
		before := time.Now()
		mutationAt = before
		result := peerCall("CreateUsageAlertingRule", req)
		auditModel[group] = append(auditModel[group], map[string]any{"createUsageAlertingRule": map[string]any{"configuration": map[string]any{"metric": map[int]string{1: "INVOCATIONS", 9: "TOTAL_UPLOAD_SIZE_BYTES", 4: "CAS_CACHE_HITS"}[metric], "window": map[int]string{1: "DAY", 2: "WEEK", 3: "MONTH"}[window], "absoluteThreshold": threshold}}})
		rule := result["usageAlertingRule"].(map[string]any)
		id := rule["metadata"].(map[string]any)["usageAlertingRuleId"].(string)
		require.NotEmpty(t, id)
		require.NotContains(t, models[group], id)
		// Assert the requested configuration independently of the read oracle.
		cfg := rule["configuration"].(map[string]any)
		require.Equal(t, threshold, fmt.Sprint(cfg["absoluteThreshold"]))
		require.Equal(t, map[int]string{1: "INVOCATIONS", 9: "TOTAL_UPLOAD_SIZE_BYTES", 4: "CAS_CACHE_HITS"}[metric], cfg["metric"])
		require.Equal(t, map[int]string{1: "DAY", 2: "WEEK", 3: "MONTH"}[window], cfg["window"])
		metadata := rule["metadata"].(map[string]any)
		timestamp, err := time.Parse(time.RFC3339Nano, metadata["createdTimestamp"].(string))
		require.NoError(t, err)
		require.True(t, !timestamp.Before(before.Add(-time.Second)) && !timestamp.After(time.Now().Add(time.Second)))
		// Organization keys have no creating user; the evaluator is not enabled.
		require.NotContains(t, metadata, "createdByUser")
		require.Equal(t, map[string]any{}, rule["status"])
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
		mutationAt = time.Now()
		grpcCall("DeleteUsageAlertingRule", req)
		auditModel[req["requestContext"].(map[string]any)["groupId"].(string)] = append(auditModel[req["requestContext"].(map[string]any)["groupId"].(string)], map[string]any{"deleteUsageAlertingRule": map[string]any{"usageAlertingRuleId": req["usageAlertingRuleId"]}})
		delete(models[item.group], item.id)
		check()
	}
	// Re-create a deleted configuration; its identity must be fresh.
	fresh := create(groups[0], 1, 1, "1000000")
	require.NotEqual(t, a, fresh)
	req := request(groups[0])
	req["usageAlertingRuleId"] = fresh
	mutationAt = time.Now()
	grpcCall("DeleteUsageAlertingRule", req)
	auditModel[req["requestContext"].(map[string]any)["groupId"].(string)] = append(auditModel[req["requestContext"].(map[string]any)["groupId"].(string)], map[string]any{"deleteUsageAlertingRule": map[string]any{"usageAlertingRuleId": req["usageAlertingRuleId"]}})
	delete(models[groups[0]], fresh)
	check()
	// Repeat valid create/delete cycles with alternating organizations and keys.
	for cycle := 0; cycle < 3; cycle++ {
		group := groups[cycle%2]
		id := create(group, 1, 1, "1000000")
		req := request(group)
		req["usageAlertingRuleId"] = id
		mutationAt = time.Now()
		grpcCall("DeleteUsageAlertingRule", req)
		auditModel[group] = append(auditModel[group], map[string]any{"deleteUsageAlertingRule": map[string]any{"usageAlertingRuleId": id}})
		delete(models[group], id)
		check()
	}

}
