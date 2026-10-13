package iprules_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
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

func TestIPRulesStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{
		harness.App:        {"--auth.ip_rules.enable=true", "--auth.ip_rules.cache_ttl=1s", "--auth.trust_xforwardedfor_header=true", "--app.client_identity.key=fixture-ip-rules-secret", "--auth.ip_rules.permitted_clients=cache-proxy", "--auth.api_key_group_cache_ttl=1s"},
		harness.CacheProxy: {"--auth.ip_rules.enable=true", "--auth.ip_rules.cache_ttl=1s", "--auth.ip_rules.remote.target=grpc://app:1985", "--auth.remote.jwt_cache_ttl=1s", "--auth.trust_xforwardedfor_header=true", "--app.client_identity.key=fixture-ip-rules-secret", "--app.client_identity.client=cache-proxy", "--app.client_identity.origin=fixture"},
	}})
	f.Web.Client.Transport = forwarded{f.Web.Client.Transport}
	// A complete valid organization URL is required by UpdateGroup, which
	// SetIPRulesConfig uses internally.
	if _, err := f.Web.RPC("UpdateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "name": "IP rules fixture", "urlIdentifier": "ip-rules-fixture"}); err != nil {
		t.Fatal(err)
	}
	key := f.Web.CreateAPIKey(t, f.GroupID, "ip-rule-admin", []string{"ORG_ADMIN", "CACHE_WRITE"})
	admin := harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	adminCtx := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", "192.0.2.10")
	requestContext := field(1, field(2, []byte(f.GroupID)))
	call := func(c context.Context, service, method string, extra []byte) ([]byte, error) {
		var out []byte
		err := admin.Conn.Invoke(c, "/"+service+"/"+method, append(append([]byte{}, requestContext...), extra...), &out, grpc.ForceCodec(wireCodec{}))
		return out, err
	}
	mutate := func(method string, extra []byte) []byte {
		t.Helper()
		out, err := call(adminCtx, "buildbuddy.service.BuildBuddyService", method, extra)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return out
	}
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	var model []rule
	enabled := false
	allowNormal := false
	check := func(step string) {
		t.Helper()
		deadline := time.NewTimer(25 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var last string
		for {
			ok := true
			for _, surface := range []string{"grpc", "http", "trusted"} {
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
				for _, ip := range []string{"192.0.2.10", "198.51.100.20"} {
					rc := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", ip)
					_, err := c.CAS.FindMissingBlobs(rc, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{harness.Digest([]byte("policy probe"))}})
					wantAllowed := !enabled || ip == "192.0.2.10" || allowNormal
					if (wantAllowed && err != nil) || (!wantAllowed && status.Code(err) != codes.PermissionDenied) {
						ok = false
						last = fmt.Sprintf("client %d ip %s allowed=%v err=%v", i, ip, wantAllowed, err)
					}
				}
			}
			if ok {
				t.Logf("%s: all projections and enforcement agree", step)
				return
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
	allowNormal = true
	check("add normal disabled")
	set(true)
	check("enable")
	set(true)
	check("enable repeated")
	normal.CIDR = "203.0.113.0/24"
	normal.Description = "updated policy"
	mutate("UpdateIPRule", field(2, normal.wire()))
	model[1] = normal
	allowNormal = false
	check("update revoke normal")
	mutate("UpdateIPRule", field(2, normal.wire()))
	check("update repeated")
	normal.CIDR = "198.51.100.20/32"
	mutate("UpdateIPRule", field(2, normal.wire()))
	model[1] = normal
	allowNormal = true
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
	allowNormal = false
	check("delete final normal")
	set(false)
	check("disable")
	set(false)
	check("disable repeated")
	mutate("DeleteIPRule", field(2, []byte(anchor.ID)))
	model = nil
	check("delete admin while disabled")
	// Audit RPC availability is explicit; enabling it requires a separate OLAP fixture.
	_, err := call(adminCtx, "buildbuddy.service.BuildBuddyService", "GetAuditLogs", nil)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("audit availability: %v", err)
	}
}
