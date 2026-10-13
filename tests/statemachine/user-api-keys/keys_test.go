package userkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// Wire field numbers are from released proto/{api_key,context}.proto and
// proto/api/v1/api_key.proto. No BuildBuddy generated dependency is needed.
type codec struct{}

func (codec) Name() string                    { return "proto" }
func (codec) Marshal(v any) ([]byte, error)   { return v.([]byte), nil }
func (codec) Unmarshal(b []byte, v any) error { *v.(*[]byte) = append([]byte(nil), b...); return nil }
func str(b []byte, n int, s string) []byte {
	return protowire.AppendString(protowire.AppendTag(b, protowire.Number(n), protowire.BytesType), s)
}
func num(b []byte, n int, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, protowire.Number(n), protowire.VarintType), v)
}

type fields map[int][][]byte

func parse(b []byte) fields {
	f := fields{}
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("invalid response tag")
		}
		b = b[k:]
		var v []byte
		switch typ {
		case protowire.BytesType:
			v, k = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			var x uint64
			x, k = protowire.ConsumeVarint(b)
			v = []byte{byte(x)}
		default:
			k = protowire.ConsumeFieldValue(n, typ, b)
		}
		if k < 0 {
			panic("invalid response field")
		}
		f[int(n)] = append(f[int(n)], v)
		b = b[k:]
	}
	return f
}
func (f fields) one(n int) string {
	if len(f[n]) == 0 {
		return ""
	}
	return string(f[n][0])
}
func rpc(c *harness.Clients, method string, b []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []byte
	err := c.Conn.Invoke(ctx, method, b, &out, grpc.ForceCodec(codec{}))
	return out, err
}

const ui = "/buildbuddy.service.BuildBuddyService/"

type key struct {
	ID, Value, Label string
	Caps             []int
	Visibility       []int
	Visible          bool
	Alive            bool
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func fromWire(b []byte) key {
	f := parse(b)
	k := key{ID: f.one(1), Value: f.one(2), Label: f.one(3), Alive: true, Visible: f.one(5) == "\x01"}
	for _, v := range f[10] {
		for _, x := range v {
			k.Visibility = append(k.Visibility, int(x))
		}
	}
	for _, v := range f[4] {
		for _, x := range v {
			k.Caps = append(k.Caps, int(x))
		}
	}
	sort.Ints(k.Caps)
	return k
}
func fromJSON(m map[string]any) key {
	k := key{Alive: true}
	k.ID, _ = m["id"].(string)
	k.Value, _ = m["value"].(string)
	k.Label, _ = m["label"].(string)
	k.Visible, _ = m["visibleToDevelopers"].(bool)
	if a, ok := m["visibility"].([]any); ok {
		for _, v := range a {
			if v == "VISIBLE_TO_GROUP_ADMINS" || v == float64(2) {
				k.Visibility = append(k.Visibility, 2)
			} else {
				k.Visibility = append(k.Visibility, -1)
			}
		}
	}
	if a, ok := m["capability"].([]any); ok {
		for _, v := range a {
			switch v {
			case "CACHE_WRITE":
				k.Caps = append(k.Caps, 1)
			case "CAS_WRITE":
				k.Caps = append(k.Caps, 4)
			default:
				if n, ok := v.(float64); ok {
					k.Caps = append(k.Caps, int(n))
				}
			}
		}
	}
	sort.Ints(k.Caps)
	return k
}
func compare(t *testing.T, want, got key, secret bool) {
	t.Helper()
	if got.Visible || !reflect.DeepEqual(got.Visibility, []int{2}) {
		t.Fatalf("unexpected user-key visibility: %v developer=%v", got.Visibility, got.Visible)
	}
	if got.ID != want.ID || got.Label != want.Label || !reflect.DeepEqual(got.Caps, want.Caps) {
		t.Fatalf("state mismatch: id=%s label=%q caps=%v; want id=%s label=%q caps=%v", got.ID, got.Label, got.Caps, want.ID, want.Label, want.Caps)
	}
	if secret && got.Value != want.Value {
		t.Fatal("single-key read changed secret")
	}
	if !secret && got.Value != "" {
		t.Fatal("list leaked secret")
	}
}

func TestUserKeyStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"user_owned_keys_enabled": true}, "auth": harness.Config{"api_key_group_cache_ttl": "1s"}}, harness.CacheProxy: {"auth": harness.Config{"remote": harness.Config{"jwt_cache_ttl": "1s"}}}}})
	_, err := f.Web.RPC("UpdateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "id": f.GroupID, "name": "user-key-state-machine", "urlIdentifier": "user-key-state-machine", "userOwnedKeysEnabled": true})
	must(t, err)
	user, _ := f.Web.UserID["id"].(string)
	if user == "" {
		t.Fatal("missing fixture user ID")
	}
	admin := f.Web.CreateAPIKey(t, f.GroupID, "local-admin", []string{"ORG_ADMIN", "CACHE_WRITE"})
	c := harness.Dial(t, f.Endpoint(harness.App).GRPC, admin)
	rc := str(nil, 2, f.GroupID)
	base := str(nil, 1, string(rc))
	webRC := f.Web.RequestContext(f.GroupID)
	model := map[string]key{}
	check := func() {
		b, e := rpc(c, ui+"GetUserApiKeys", str(base, 3, user))
		must(t, e)
		listed := map[string]key{}
		for _, v := range parse(b)[2] {
			k := fromWire(v)
			listed[k.ID] = k
		}
		res, e := f.Web.RPC("GetUserApiKeys", map[string]any{"requestContext": webRC, "userId": user})
		must(t, e)
		web := map[string]key{}
		if a, ok := res["apiKey"].([]any); ok {
			for _, v := range a {
				k := fromJSON(v.(map[string]any))
				web[k.ID] = k
			}
		}
		gb, ge := rpc(c, ui+"GetApiKeys", base)
		must(t, ge)
		for _, v := range parse(gb)[2] {
			if _, ok := model[fromWire(v).ID]; ok {
				t.Fatal("user key leaked into org-key list")
			}
		}
		gw, ge := f.Web.RPC("GetApiKeys", map[string]any{"requestContext": webRC})
		must(t, ge)
		if a, ok := gw["apiKey"].([]any); ok {
			for _, v := range a {
				if _, found := model[fromJSON(v.(map[string]any)).ID]; found {
					t.Fatal("user key leaked into web org-key list")
				}
			}
		}
		alive := 0
		for id, k := range model {
			if k.Alive {
				alive++
				compare(t, k, listed[id], false)
				compare(t, k, web[id], false)
			} else {
				if _, ok := listed[id]; ok {
					t.Fatal("deleted key still listed")
				}
				if _, ok := web[id]; ok {
					t.Fatal("deleted key still web-listed")
				}
			}
			b, e := rpc(c, ui+"GetUserApiKey", str(base, 2, id))
			wr, we := f.Web.RPC("GetUserApiKey", map[string]any{"requestContext": webRC, "apiKeyId": id})
			if k.Alive {
				must(t, e)
				must(t, we)
				compare(t, k, fromWire(parse(b)[2][0]), true)
				compare(t, k, fromJSON(wr["apiKey"].(map[string]any)), true)
			} else {
				if status.Code(e) != codes.NotFound || we == nil {
					t.Fatalf("deleted key read: grpc=%v web=%v", e, we)
				}
			}
		}
		if len(listed) != alive || len(web) != alive {
			t.Fatalf("list sizes grpc=%d web=%d model=%d", len(listed), len(web), alive)
		}
	}
	probeSequence := 0
	capabilityCheck := func(client *harness.Clients, component harness.Component, k key) {
		wantCAS, wantAC := len(k.Caps) > 0, false
		for _, cap := range k.Caps {
			if cap == 1 {
				wantAC = true
			}
		}
		readers := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
		deadline := time.Now().Add(20 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			probeSequence++
			payload := []byte(fmt.Sprintf("%s-%s-%d", k.ID, component, probeSequence))
			d := harness.Digest(payload)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			r, e := client.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: payload}}})
			must(t, e)
			if len(r.Responses) != 1 || r.Responses[0].GetStatus().GetCode() != 0 {
				t.Fatal("valid CAS write failed")
			}
			ad := harness.Digest(append([]byte("action-"), payload...))
			_, e = client.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: ad, ActionResult: &repb.ActionResult{StdoutRaw: payload}})
			must(t, e)
			converged := true
			for _, reader := range readers {
				missing, e := reader.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
				must(t, e)
				if (len(missing.MissingBlobDigests) == 0) != wantCAS {
					converged = false
				}
				ar, e := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: ad, InlineStdout: true})
				if wantAC {
					if e != nil || !bytes.Equal(ar.GetStdoutRaw(), payload) {
						converged = false
					}
				} else if status.Code(e) != codes.NotFound {
					converged = false
				}
			}
			cancel()
			if converged {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s capabilities %v failed to converge on app and proxy", component, k.Caps)
			}
			<-ticker.C
		}
	}

	auth := func(k key, live bool) {
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			client := harness.Dial(t, f.Endpoint(component).GRPC, k.Value)
			if live && component == harness.App {
				b, e := rpc(client, ui+"GetUser", base)
				must(t, e)
				display := parse(parse(b)[1][0])
				if parse(display[1][0]).one(1) != user {
					t.Fatal("key authenticated as wrong owner")
				}
			}
			deadline := time.Now().Add(20 * time.Second)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, e := client.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{harness.Digest([]byte("auth-probe"))}})
				cancel()
				if live && e == nil || !live && (status.Code(e) == codes.Unauthenticated || status.Code(e) == codes.PermissionDenied) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s live=%v auth failed to converge: %v", component, live, e)
				}
				<-ticker.C
			}
			if live {
				capabilityCheck(client, component, k)
			}
		}
	}
	check()
	for _, path := range []string{"ui-grpc", "public-grpc", "public-http"} {
		// Same-label repeated creates are valid distinct keys, not idempotent creates.
		for repeat := 0; repeat < 2; repeat++ {
			label := path
			var k key
			if path == "ui-grpc" {
				req := str(str(base, 6, user), 3, label)
				req = num(req, 4, 1)
				b, e := rpc(c, ui+"CreateUserApiKey", req)
				must(t, e)
				k = fromWire(parse(b)[2][0])
			} else if path == "public-grpc" {
				b, e := rpc(c, "/api.v1.ApiService/CreateUserApiKey", str(str(nil, 1, user), 2, label))
				must(t, e)
				p := parse(parse(b)[1][0])
				k = key{ID: p.one(1), Value: p.one(2), Label: p.one(3), Caps: []int{1}, Alive: true}
			} else {
				body, _ := json.Marshal(map[string]any{"user_id": user, "label": label})
				req, e := http.NewRequest("POST", f.Endpoint(harness.App).HTTP+"/api/v1/CreateUserApiKey", bytes.NewReader(body))
				must(t, e)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-buildbuddy-api-key", admin)
				client := &http.Client{Timeout: 10 * time.Second}
				r, e := client.Do(req)
				must(t, e)
				data, e := io.ReadAll(r.Body)
				r.Body.Close()
				must(t, e)
				if r.StatusCode != 200 {
					t.Fatalf("public HTTP %d: %s", r.StatusCode, data)
				}
				var out struct {
					ApiKey struct {
						ID    string `json:"apiKeyId"`
						Value string `json:"value"`
						Label string `json:"label"`
					} `json:"apiKey"`
				}
				must(t, json.Unmarshal(data, &out))
				k = key{ID: out.ApiKey.ID, Value: out.ApiKey.Value, Label: out.ApiKey.Label, Caps: []int{1}, Alive: true}
			}
			if k.ID == "" || k.Value == "" {
				t.Fatal("empty create response")
			}
			if _, ok := model[k.ID]; ok {
				t.Fatal("repeated create reused ID")
			}
			expectedCaps := []int{1}
			if k.Label != label || !reflect.DeepEqual(k.Caps, expectedCaps) {
				t.Fatal("create response did not match requested label or expected capabilities")
			}
			model[k.ID] = k
			check()
			auth(k, true)
		}
	}
	ids := make([]string, 0, len(model))
	for id := range model {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		k := model[id]
		for _, caps := range [][]int{{4}, {1}, {}} {
			k.Label = "updated-" + id
			k.Caps = caps
			if len(caps) == 0 {
				k.Label = ""
				k.Caps = nil
			}
			req := str(str(base, 2, id), 3, k.Label)
			for _, cap := range caps {
				req = num(req, 4, uint64(cap))
			}
			for repeat := 0; repeat < 2; repeat++ {
				_, e := rpc(c, ui+"UpdateUserApiKey", req)
				must(t, e)
				model[id] = k
				check()
				auth(k, true)
			}
		}
		_, e := rpc(c, ui+"DeleteUserApiKey", str(base, 2, id))
		must(t, e)
		k.Alive = false
		model[id] = k
		check()
		auth(k, false)
	}
	t.Logf("validated %d keys across three creation transports", len(model))
}
