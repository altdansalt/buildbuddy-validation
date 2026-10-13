package iprules_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// Wire fields mirror released proto/iprules.proto and proto/context.proto.
// Using the standard protobuf codec name avoids adding generated app protos to
// the shared dependency scaffold.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func fields(t *testing.T, b []byte, n protowire.Number) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		num, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			t.Fatal("bad protobuf tag")
		}
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				t.Fatal("bad protobuf bytes")
			}
			if num == n {
				out = append(out, v)
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(num, typ, b)
			if k < 0 {
				t.Fatal("bad protobuf field")
			}
			b = b[k:]
		}
	}
	return out
}

type rule struct{ ID, CIDR, Description string }

func (r rule) wire() []byte {
	b := field(1, []byte(r.ID))
	b = append(b, field(2, []byte(r.CIDR))...)
	return append(b, field(3, []byte(r.Description))...)
}
func decodeRule(t *testing.T, b []byte) rule {
	vals := []string{"", "", ""}
	for i := range vals {
		f := fields(t, b, protowire.Number(i+1))
		if len(f) > 0 {
			vals[i] = string(f[0])
		}
	}
	return rule{vals[0], vals[1], vals[2]}
}
func sorted(rs []rule) []rule {
	out := append([]rule{}, rs...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type forwarded struct{ base http.RoundTripper }
type auditForwarded struct {
	base http.RoundTripper
	ip   string
}

func (f auditForwarded) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", f.ip)
	return f.base.RoundTrip(r)
}

func (f forwarded) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", "192.0.2.10")
	return f.base.RoundTrip(r)
}
func identity() string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"Client": "cache-proxy", "Origin": "fixture", "exp": time.Now().Add(time.Hour).Unix()})
	s := enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload)
	h := hmac.New(sha256.New, []byte("fixture-ip-rules-secret"))
	h.Write([]byte(s))
	return s + "." + enc(h.Sum(nil))
}

var defaultRuleTTL = flag.Bool("ip_rules_default_ttl", false, "Exercise a shorter sequence with the released default rule TTL")

func TestIPRulesStateMachine(t *testing.T) {
	olapNetwork := startOLAP(t)
	opts := harness.Options{DockerArgs: map[harness.Component][]string{harness.App: {"--network", olapNetwork}}, Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{
		harness.App:        {"--app.client_identity.client=app", "--app.client_identity.origin=fixture", "--app.audit_logs_enabled=true", "--olap_database.data_source=clickhouse://default:@ip-rules-olap:9000/buildbuddy_test", "--app.user_owned_keys_enabled=true", "--auth.ip_rules.enable=true", "--auth.ip_rules.cache_ttl=5s", "--auth.trust_xforwardedfor_header=true", "--app.client_identity.key=fixture-ip-rules-secret", "--auth.ip_rules.permitted_clients=cache-proxy", "--auth.api_key_group_cache_ttl=1s"},
		harness.CacheProxy: {"--auth.ip_rules.enable=true", "--auth.ip_rules.cache_ttl=5s", "--auth.ip_rules.remote.target=grpc://app:1985", "--auth.remote.jwt_cache_ttl=1s", "--auth.trust_xforwardedfor_header=true", "--app.client_identity.key=fixture-ip-rules-secret", "--app.client_identity.client=cache-proxy", "--app.client_identity.origin=fixture"},
	}}
	if *defaultRuleTTL {
		for component, args := range opts.Args {
			filtered := []string{}
			for _, arg := range args {
				if !strings.HasPrefix(arg, "--auth.ip_rules.cache_ttl=") {
					filtered = append(filtered, arg)
				}
			}
			opts.Args[component] = filtered
		}
	}
	f := harness.Start(t, opts)
	f.Web.Client.Transport = forwarded{f.Web.Client.Transport}
	// A complete valid organization URL is required by UpdateGroup, which
	// SetIPRulesConfig uses internally.
	if _, err := f.Web.RPC("UpdateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "name": "IP rules fixture", "urlIdentifier": "ip-rules-fixture", "userOwnedKeysEnabled": true}); err != nil {
		t.Fatal(err)
	}
	key2 := f.Web.CreateAPIKey(t, f.GroupID, "second-client", []string{"ORG_ADMIN", "CACHE_WRITE"})
	key := f.Web.CreateAPIKey(t, f.GroupID, "ip-rule-admin", []string{"ORG_ADMIN", "CACHE_WRITE"})
	admin := harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	adminCtx := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", "192.0.2.10")
	requestContext := field(1, field(2, []byte(f.GroupID)))
	call := func(c context.Context, service, method string, extra []byte) ([]byte, error) {
		var out []byte
		err := admin.Conn.Invoke(c, "/"+service+"/"+method, append(append([]byte{}, requestContext...), extra...), &out, grpc.ForceCodec(wireCodec{}))
		return out, err
	}
	type event struct {
		Method    string
		Extra     []byte
		ID, Name  string
		Action    uint64
		Actor, IP string
	}
	var events []event
	var model []rule
	mutate := func(method string, extra []byte) []byte {
		t.Helper()
		mc := adminCtx
		if len(events)%2 == 1 {
			mc = harness.WithAPIKey(mc, key2)
		}
		out, err := call(mc, "buildbuddy.service.BuildBuddyService", method, extra)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		ev := event{Method: method, Extra: append([]byte{}, extra...), Action: map[string]uint64{"AddIPRule": 1, "UpdateIPRule": 4, "DeleteIPRule": 5, "SetIPRulesConfig": 13}[method], Actor: "ip-rule-admin", IP: "192.0.2.10"}
		if len(events)%2 == 1 {
			ev.Actor = "second-client"
		}
		if method == "AddIPRule" {
			r := decodeRule(t, fields(t, out, 2)[0])
			ev.ID = r.ID
			ev.Name = r.Description
			ev.Extra = field(2, r.wire())
		}
		if method == "UpdateIPRule" {
			ev.ID = decodeRule(t, fields(t, extra, 2)[0]).ID
		}
		if method == "DeleteIPRule" {
			ev.ID = string(fields(t, extra, 2)[0])
		}
		for _, r := range model {
			if r.ID == ev.ID {
				ev.Name = r.Description
			}
		}
		events = append(events, ev)
		return out
	}
	userKeyResponse, err := f.Web.RPC("CreateUserApiKey", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "label": "user-projection", "capability": []string{"CACHE_WRITE"}})
	if err != nil {
		t.Fatal(err)
	}
	userKey := userKeyResponse["apiKey"].(map[string]any)["value"].(string)
	userClient := harness.Dial(t, f.Endpoint(harness.App).GRPC, userKey)
	checkLogin := loginChecks(t, userClient, userKey)
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy), harness.Dial(t, f.Endpoint(harness.App).GRPC, key2), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key2)}
	invocationID := seedInvocation(t, clients[0], adminCtx)
	probeBytes := []byte("ip-rules cross-surface streaming and download probe")
	probeDigest, err := clients[1].Upload(adminCtx, "", probeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients[0].Upload(adminCtx, "", probeBytes); err != nil {
		t.Fatal(err)
	}
	enabled := false
	allowed := func(ip string) bool {
		if !enabled {
			return true
		}
		for _, r := range model {
			_, network, err := net.ParseCIDR(r.CIDR)
			if err != nil {
				t.Fatal(err)
			}
			if network.Contains(net.ParseIP(ip)) {
				return true
			}
		}
		return false
	}
	check := func(step string) {
		t.Helper()
		bound := 25 * time.Second
		if *defaultRuleTTL {
			bound = 150 * time.Second
		}
		deadline := time.NewTimer(bound)
		defer deadline.Stop()
		interval := 250 * time.Millisecond
		if *defaultRuleTTL {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var last string
		start := time.Now()
		var converged time.Time
		for {
			ok := true
			for _, surface := range []string{"grpc", "grpc-second", "http", "trusted"} {
				var got []rule
				var err error
				if surface == "http" {
					var rsp map[string]any
					rsp, err = f.Web.RPC("GetIPRules", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
					if err == nil {
						if entries, yes := rsp["ipRules"].([]any); yes {
							for _, v := range entries {
								r := v.(map[string]any)
								id, _ := r["ipRuleId"].(string)
								cidr, _ := r["cidr"].(string)
								d, _ := r["description"].(string)
								got = append(got, rule{id, cidr, d})
							}
						}
					}
				} else {
					c := adminCtx
					if surface == "grpc-second" {
						c = harness.WithAPIKey(c, key2)
					}
					service := "buildbuddy.service.BuildBuddyService"
					if surface == "trusted" {
						c = metadata.AppendToOutgoingContext(c, "x-buildbuddy-client-identity", identity())
						service = "iprules.IPRulesService"
					}
					var b []byte
					b, err = call(c, service, "GetIPRules", nil)
					if err == nil {
						for _, v := range fields(t, b, 2) {
							got = append(got, decodeRule(t, v))
						}
					}
				}
				want := model
				if surface == "trusted" && !enabled {
					want = nil
				}
				if err != nil || !reflect.DeepEqual(sorted(got), sorted(want)) {
					ok = false
					last = fmt.Sprintf("%s rules got %v want %v err %v", surface, got, want, err)
				}
			}
			b, err := call(adminCtx, "buildbuddy.service.BuildBuddyService", "GetIPRulesConfig", nil)
			grpcEnabled := false
			for len(b) > 0 {
				n, typ, k := protowire.ConsumeTag(b)
				if k < 0 {
					t.Fatal("bad config")
				}
				b = b[k:]
				if n == 2 && typ == protowire.VarintType {
					v, k := protowire.ConsumeVarint(b)
					grpcEnabled = v != 0
					b = b[k:]
				} else {
					k := protowire.ConsumeFieldValue(n, typ, b)
					if k < 0 {
						t.Fatal("bad config value")
					}
					b = b[k:]
				}
			}
			rsp, httpErr := f.Web.RPC("GetIPRulesConfig", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
			httpEnabled, _ := rsp["enforceIpRules"].(bool)
			if err != nil || httpErr != nil || grpcEnabled != enabled || httpEnabled != enabled {
				ok = false
				last = fmt.Sprintf("config grpc=%v http=%v want=%v errors=%v/%v", grpcEnabled, httpEnabled, enabled, err, httpErr)
			}
			user, userErr := f.Web.RPC("GetUser", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
			foundGroup := false
			if groups, yes := user["userGroup"].([]any); yes {
				for _, g := range groups {
					group := g.(map[string]any)
					if group["id"] == f.GroupID {
						foundGroup = true
						flag, _ := group["enforceIpRules"].(bool)
						if flag != enabled {
							ok = false
							last = fmt.Sprintf("GetUser group enforcement=%v want=%v", flag, enabled)
						}
					}
				}
			}
			if userErr != nil || !foundGroup {
				ok = false
				last = fmt.Sprintf("GetUser projection missing: %v %v", user, userErr)
			}
			for i, c := range clients {
				for _, ip := range []string{"192.0.2.10", "198.51.100.20", "203.0.113.42", "203.0.114.42"} {
					rc := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", ip)
					_, err := c.CAS.FindMissingBlobs(rc, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{harness.Digest([]byte("policy probe"))}})
					wantAllowed := allowed(ip)
					read, readErr := c.Read(rc, "", probeDigest)
					if (wantAllowed && (readErr != nil || !bytes.Equal(read, probeBytes))) || (!wantAllowed && status.Code(readErr) != codes.PermissionDenied) {
						ok = false
						last = fmt.Sprintf("streaming read client=%d ip=%s allowed=%v err=%v bytes=%q", i, ip, wantAllowed, readErr, read)
					}
					if (wantAllowed && err != nil) || (!wantAllowed && status.Code(err) != codes.PermissionDenied) {
						ok = false
						last = fmt.Sprintf("client %d ip %s allowed=%v err=%v", i, ip, wantAllowed, err)
					}
				}
			}

			// Audit the omitted UI access projection and HTTP authorization,
			// using the same user, group, and IPs as the CAS probes.
			for _, ip := range []string{"192.0.2.10", "198.51.100.20", "203.0.113.42", "203.0.114.42"} {
				originalTransport := f.Web.Client.Transport
				f.Web.Client.Transport = auditForwarded{originalTransport.(forwarded).base, ip}
				user, userErr := f.Web.RPC("GetUser", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
				ipConfig, configErr := f.Web.RPC("GetIPRulesConfig", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
				if (allowed(ip) && configErr != nil) || (!allowed(ip) && (configErr == nil || !strings.Contains(configErr.Error(), "HTTP 403:"))) {
					ok = false
					last = fmt.Sprintf("HTTP config authorization ip=%s response=%v err=%v", ip, ipConfig, configErr)
				}
				list, listErr := f.Web.RPC("GetIPRules", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
				downloadURL := f.Web.URL + "/file/download?bytestream_url=" + url.QueryEscape("bytestream://"+url.User(key).String()+"@app:1985/"+harness.ResourceName("", probeDigest, false))
				downloadRequest, _ := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
				downloadRequest.Header.Set("x-buildbuddy-api-key", key)
				dr, de := f.Web.Client.Do(downloadRequest)
				var db []byte
				var dc int
				if de == nil {
					dc = dr.StatusCode
					db, de = io.ReadAll(dr.Body)
					dr.Body.Close()
				}
				// Known missing HTTP IP check: app-signed internal cache reads succeed for revoked external IPs.
				if de != nil || dc != 200 || !bytes.Equal(db, probeBytes) {
					ok = false
					last = fmt.Sprintf("download ip=%s allowed=%v status=%d err=%v body=%q", ip, allowed(ip), dc, de, db)
				}
				for _, artifact := range []string{"raw_json", "buildlog"} {
					rq, _ := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?invocation_id="+invocationID+"&attempt=1&artifact="+artifact, nil)
					rq.Header.Set("x-buildbuddy-api-key", key)
					r, e := f.Web.Client.Do(rq)
					var body []byte
					var code int
					if e == nil {
						code = r.StatusCode
						body, e = io.ReadAll(r.Body)
						r.Body.Close()
					}
					marker := []byte(strings.TrimSpace(logMarker))
					if artifact == "raw_json" {
						marker = []byte(`"command":"build"`)
					}
					// Known divergence: blobstore downloads bypass IP checks. The isolated
					// bug target asserts 403; normal CI pins actual behavior after every step.
					if e != nil || code != 200 || !bytes.Contains(body, marker) {
						ok = false
						last = fmt.Sprintf("artifact=%s ip=%s allowed=%v status=%d err=%v body=%q", artifact, ip, allowed(ip), code, e, body)
					}
				}
				hl, herr := f.Web.RPC("GetEventLogChunk", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": invocationID})
				buffer, _ := hl["buffer"].(string)
				decoded, _ := base64.StdEncoding.DecodeString(buffer)
				if (allowed(ip) && (herr != nil || !bytes.Contains(decoded, []byte(strings.TrimSpace(logMarker))))) || (!allowed(ip) && (herr == nil || !strings.Contains(herr.Error(), "HTTP 403:"))) {
					ok = false
					last = fmt.Sprintf("HTTP event log ip=%s response=%v err=%v", ip, hl, herr)
				}
				hi, hie := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": invocationID}})
				invsHTTP, _ := hi["invocation"].([]any)
				validID := len(invsHTTP) == 1
				if validID {
					v, _ := invsHTTP[0].(map[string]any)
					validID = v["invocationId"] == invocationID
				}
				if (allowed(ip) && (hie != nil || !validID)) || (!allowed(ip) && (hie == nil || !strings.Contains(hie.Error(), "HTTP 403:"))) {
					ok = false
					last = fmt.Sprintf("HTTP invocation metadata ip=%s err=%v", ip, hie)
				}
				apiReq, _ := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/GetFile", strings.NewReader(fmt.Sprintf(`{"uri":%q}`, "bytestream://"+url.User(key).String()+"@app:1985/"+harness.ResourceName("", probeDigest, false))))
				apiReq.Header.Set("Content-Type", "application/json")
				apiReq.Header.Set("x-buildbuddy-api-key", key)
				apiRsp, apiErr := f.Web.Client.Do(apiReq)
				var apiBody []byte
				var apiCode int
				if apiErr == nil {
					apiCode = apiRsp.StatusCode
					apiBody, apiErr = io.ReadAll(apiRsp.Body)
					apiRsp.Body.Close()
				}
				if (allowed(ip) && (apiErr != nil || apiCode != 200 || !bytes.Equal(apiBody, probeBytes))) || (!allowed(ip) && apiCode != 403) {
					ok = false
					last = fmt.Sprintf("API GetFile ip=%s allowed=%v status=%d err=%v body=%q", ip, allowed(ip), apiCode, apiErr, apiBody)
				}
				f.Web.Client.Transport = originalTransport
				wantAllowed := allowed(ip)
				gc := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", ip)
				ib, ie := call(gc, "buildbuddy.service.BuildBuddyService", "GetInvocation", field(2, field(1, []byte(invocationID))))
				invs := fields(t, ib, 2)
				iid := [][]byte{}
				if len(invs) == 1 {
					iid = fields(t, invs[0], 1)
				}
				if (wantAllowed && (ie != nil || len(iid) != 1 || string(iid[0]) != invocationID)) || (!wantAllowed && status.Code(ie) != codes.PermissionDenied) {
					ok = false
					last = fmt.Sprintf("grpc invocation metadata ip=%s err=%v id=%q", ip, ie, iid)
				}
				lb, le := call(gc, "buildbuddy.service.BuildBuddyService", "GetEventLogChunk", field(2, []byte(invocationID)))
				logBuffers := fields(t, lb, 4)
				if (wantAllowed && (le != nil || len(logBuffers) != 1 || !bytes.Contains(logBuffers[0], []byte(strings.TrimSpace(logMarker))))) || (!wantAllowed && status.Code(le) != codes.PermissionDenied) {
					ok = false
					last = fmt.Sprintf("grpc log ip=%s err=%v buffers=%q", ip, le, logBuffers)
				}
				sc, scancel := context.WithTimeout(gc, 5*time.Second)
				ss, se := admin.Conn.NewStream(sc, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetEventLog", grpc.ForceCodec(wireCodec{}))
				var streamLog []byte
				if se == nil {
					se = ss.SendMsg(append(append([]byte{}, requestContext...), field(2, []byte(invocationID))...))
					if se == nil {
						se = ss.CloseSend()
					}
					if se == nil {
						for {
							var msg []byte
							se = ss.RecvMsg(&msg)
							if se != nil {
								break
							}
							for _, buf := range fields(t, msg, 4) {
								streamLog = append(streamLog, buf...)
							}
						}
					}
				}
				scancel()
				if (wantAllowed && (se != io.EOF || !bytes.Contains(streamLog, []byte(strings.TrimSpace(logMarker))))) || (!wantAllowed && status.Code(se) != codes.PermissionDenied) {
					ok = false
					last = fmt.Sprintf("streaming log ip=%s err=%v buffer=%q", ip, se, streamLog)
				}
				var userWire []byte
				uc := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", ip)
				grpcErr := userClient.Conn.Invoke(uc, "/buildbuddy.service.BuildBuddyService/GetUser", field(2, field(2, []byte(f.GroupID))), &userWire, grpc.ForceCodec(wireCodec{}))
				selectedWire := fields(t, userWire, 9)
				wantAccessWire := byte(1)
				if !wantAllowed {
					wantAccessWire = 3
				}
				if (wantAllowed && (grpcErr != nil || len(selectedWire) != 1 || !reflect.DeepEqual(selectedWire[0], append(field(1, []byte(f.GroupID)), 0x10, wantAccessWire)))) || (!wantAllowed && status.Code(grpcErr) != codes.PermissionDenied) {
					ok = false
					last = fmt.Sprintf("grpc GetUser ip=%s wire=%x err=%v", ip, userWire, grpcErr)
				}
				found := false
				for _, g := range fields(t, userWire, 2) {
					ids := fields(t, g, 1)
					if len(ids) == 1 && string(ids[0]) == f.GroupID {
						found = true
						if varint(g, 14) != boolInt(enabled) {
							ok = false
							last = "grpc GetUser enforcement mismatch"
						}
					}
				}
				if wantAllowed && !found {
					ok = false
					last = "grpc GetUser group missing"
				}
				selected, selectedOK := user["selectedGroup"].(map[string]any)
				wantAccess := "ALLOWED"
				if !wantAllowed {
					wantAccess = "DENIED_BY_IP_RULES"
				}
				if userErr != nil || !selectedOK || selected["groupId"] != f.GroupID || selected["access"] != wantAccess {
					ok = false
					last = fmt.Sprintf("audit GetUser ip=%s selected=%v want access=%s err=%v", ip, selected, wantAccess, userErr)
				}
				if (wantAllowed && listErr != nil) || (!wantAllowed && (listErr == nil || !strings.Contains(listErr.Error(), "HTTP 403:") || !strings.Contains(listErr.Error(), "IP rules"))) {
					ok = false
					last = fmt.Sprintf("audit HTTP GetIPRules ip=%s allowed=%v response=%v err=%v", ip, wantAllowed, list, listErr)
				}
			}
			after := time.Now().Add(-time.Hour)
			before := time.Now().Add(time.Hour)
			timestamp := func(tm time.Time) []byte {
				return protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), uint64(tm.Unix()))
			}
			ab, ae := call(adminCtx, "buildbuddy.service.BuildBuddyService", "GetAuditLogs", append(field(3, timestamp(after)), field(4, timestamp(before))...))
			ar, he := f.Web.RPC("GetAuditLogs", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "timestampAfter": after.Format(time.RFC3339Nano), "timestampBefore": before.Format(time.RFC3339Nano)})
			grpcEvents := []event{}
			httpEvents := []event{}
			methodFields := map[protowire.Number]string{15: "AddIPRule", 16: "UpdateIPRule", 17: "DeleteIPRule", 18: "SetIPRulesConfig"}
			for _, e := range fields(t, ab, 2) {
				requests := fields(t, e, 5)
				if len(requests) != 1 {
					continue
				}
				api := fields(t, requests[0], 1)
				if len(api) != 1 {
					continue
				}
				for num, method := range methodFields {
					rs := fields(t, api[0], num)
					if len(rs) != 1 {
						continue
					}
					resources := fields(t, e, 3)
					ev := event{Method: method, Action: varint(e, 4)}
					auth := fields(t, e, 2)
					if len(auth) == 1 {
						ip := fields(t, auth[0], 3)
						if len(ip) == 1 {
							ev.IP = string(ip[0])
						}
						ak := fields(t, auth[0], 2)
						if len(ak) == 1 {
							label := fields(t, ak[0], 2)
							if len(label) == 1 {
								ev.Actor = string(label[0])
							}
						}
					}
					if len(resources) > 0 {
						id := fields(t, resources[0], 2)
						name := fields(t, resources[0], 3)
						if len(id) > 0 {
							ev.ID = string(id[0])
						}
						if len(name) > 0 {
							ev.Name = string(name[0])
						}
					}
					// Audit logger clears request_context. Compare remaining mutation payload exactly.
					ev.Extra = rs[0]
					grpcEvents = append(grpcEvents, ev)
				}
			}
			if entries, yes := ar["entries"].([]any); yes {
				for _, v := range entries {
					e := v.(map[string]any)
					req, _ := e["request"].(map[string]any)
					api, _ := req["apiRequest"].(map[string]any)
					for _, method := range methodFields {
						jsonName := map[string]string{"AddIPRule": "addIpRule", "UpdateIPRule": "updateIpRule", "DeleteIPRule": "deleteIpRule", "SetIPRulesConfig": "setRulesConfig"}[method]
						payload, yes := api[jsonName].(map[string]any)
						if !yes {
							continue
						}
						ev := event{Method: method}
						action, _ := e["action"].(string)
						ev.Action = map[string]uint64{"CREATE": 1, "UPDATE": 4, "DELETE": 5, "UPDATE_IP_RULES_CONFIG": 13}[action]
						auth, _ := e["authenticationInfo"].(map[string]any)
						ev.IP, _ = auth["clientIp"].(string)
						ak, _ := auth["apiKey"].(map[string]any)
						ev.Actor, _ = ak["label"].(string)
						resource, _ := e["resource"].(map[string]any)
						ev.ID, _ = resource["id"].(string)
						ev.Name, _ = resource["name"].(string)
						switch method {
						case "AddIPRule", "UpdateIPRule":
							r, _ := payload["rule"].(map[string]any)
							id, _ := r["ipRuleId"].(string)
							cidr, _ := r["cidr"].(string)
							desc, _ := r["description"].(string)
							ev.Extra = field(2, rule{id, cidr, desc}.wire())
						case "DeleteIPRule":
							id, _ := payload["ipRuleId"].(string)
							ev.Extra = field(2, []byte(id))
						case "SetIPRulesConfig":
							flag, _ := payload["enforceIpRules"].(bool)
							ev.Extra = []byte{0x10, byte(boolInt(flag))}
						}
						httpEvents = append(httpEvents, ev)
					}
				}
			}
			// OLAP returns chronological order, including repeated operations.
			expected := append([]event{}, events...)
			normalize := func(es []event) []event {
				for i := range es {
					if es[i].Method == "SetIPRulesConfig" {
						es[i].ID = ""
						es[i].Name = ""
						es[i].Extra = []byte{0x10, byte(varint(es[i].Extra, 2))}
					} else if es[i].Method == "AddIPRule" || es[i].Method == "UpdateIPRule" {
						r := fields(t, es[i].Extra, 2)
						if len(r) == 1 {
							es[i].Extra = field(2, decodeRule(t, r[0]).wire())
						}
					}
				}
				return es
			}
			if ae != nil || he != nil || !reflect.DeepEqual(normalize(grpcEvents), normalize(expected)) || !reflect.DeepEqual(normalize(httpEvents), normalize(append([]event{}, events...))) {
				ok = false
				last = fmt.Sprintf("audit grpc=%v http=%v want=%v err=%v/%v", grpcEvents, httpEvents, events, ae, he) + fmt.Sprintf(" HTTP raw=%v", ar)
			}
			if ok {
				if converged.IsZero() {
					converged = time.Now()
				}
				if time.Since(converged) >= 6*time.Second {
					for _, ip := range []string{"192.0.2.10", "198.51.100.20", "203.0.113.42", "203.0.114.42"} {
						checkLogin(ip, allowed(ip))
					}
					ttl := "5s"
					if *defaultRuleTTL {
						ttl = "released default 2m"
					}
					t.Logf("%s: convergence=%s stable=%s (rule TTL=%s)", step, converged.Sub(start), time.Since(converged), ttl)
					return
				}
			}
			if !ok {
				converged = time.Time{}
			}
			select {
			case <-deadline.C:
				t.Fatalf("%s: %s", step, last)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-ticker.C:
			}
		}
	}
	add := func(cidr, desc string) rule {
		b := mutate("AddIPRule", field(2, rule{CIDR: cidr, Description: desc}.wire()))
		rs := fields(t, b, 2)
		if len(rs) != 1 {
			t.Fatalf("missing add detail: %x", b)
		}
		r := decodeRule(t, rs[0])
		if r.ID == "" || r.CIDR != cidr || r.Description != desc {
			t.Fatalf("add detail: %v", r)
		}
		model = append(model, r)
		return r
	}
	set := func(v bool) {
		b := []byte{0x10, 0}
		if v {
			b[1] = 1
		}
		mutate("SetIPRulesConfig", b)
		enabled = v
	}
	check("initial")
	anchor := add("192.0.2.10/32", "admin access")
	check("add admin")
	normal := add("198.51.100.20/32", "normal access")
	check("add normal disabled")
	set(true)
	check("enable")
	if *defaultRuleTTL {
		normal.CIDR = "203.0.113.0/24"
		normal.Description = "default TTL replacement"
		mutate("UpdateIPRule", field(2, normal.wire()))
		model[1] = normal
		check("default TTL update revoke and grant subnet")
		mutate("DeleteIPRule", field(2, []byte(normal.ID)))
		model = []rule{anchor}
		check("default TTL delete revoke subnet")
		return
	}
	set(true)
	check("enable repeated")
	normal.CIDR = "203.0.113.0/24"
	normal.Description = "updated policy"
	mutate("UpdateIPRule", field(2, normal.wire()))
	model[1] = normal
	check("update revoke normal")
	mutate("UpdateIPRule", field(2, normal.wire()))
	check("update repeated")
	normal.CIDR = "198.51.100.20/32"
	mutate("UpdateIPRule", field(2, normal.wire()))
	model[1] = normal
	check("update restore normal")
	duplicate := add(normal.CIDR, normal.Description)
	if duplicate.ID == normal.ID {
		t.Fatal("repeated add must create a distinct entry")
	}
	check("repeat add")
	mutate("DeleteIPRule", field(2, []byte(normal.ID)))
	model = []rule{anchor, duplicate}
	check("delete one duplicate")
	mutate("DeleteIPRule", field(2, []byte(duplicate.ID)))
	model = []rule{anchor}
	check("delete final normal")
	recreated := add("198.51.100.20/32", "recreated after deletion")
	check("recreate")
	mutate("DeleteIPRule", field(2, []byte(recreated.ID)))
	model = []rule{anchor}
	check("delete recreated")
	set(false)
	check("disable")
	set(false)
	check("disable repeated")
	mutate("DeleteIPRule", field(2, []byte(anchor.ID)))
	model = nil
	check("delete admin while disabled")

}

func boolInt(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}
func varint(b []byte, wanted protowire.Number) uint64 {
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			return 999
		}
		b = b[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				return 999
			}
			if n == wanted {
				return v
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			if k < 0 {
				return 999
			}
			b = b[k:]
		}
	}
	return 0
}
