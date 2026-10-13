package orgkeys

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// Minimal wire bindings to the released proto/api_key.proto, context.proto and
// bazel_config.proto. Avoid importing (and building) the product source tree.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func str(n protowire.Number, s string) []byte { return msg(n, []byte(s)) }
func msg(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func num(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func fields(b []byte) map[protowire.Number][][]byte {
	out := map[protowire.Number][][]byte{}
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
			v = protowire.AppendVarint(nil, x)
		default:
			k = protowire.ConsumeFieldValue(n, typ, b)
		}
		if k < 0 {
			panic("invalid response field")
		}
		out[n] = append(out[n], v)
		b = b[k:]
	}
	return out
}
func first(m map[protowire.Number][][]byte, n protowire.Number) []byte {
	if len(m[n]) == 0 {
		return nil
	}
	return m[n][0]
}

type key struct {
	id, value, label string
	caps             []uint64
	visible          bool
}

func decode(b []byte) key {
	m := fields(b)
	k := key{id: string(first(m, 1)), value: string(first(m, 2)), label: string(first(m, 3))}
	for _, b := range m[4] {
		for len(b) > 0 {
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				panic("bad capability")
			}
			k.caps = append(k.caps, v)
			b = b[n:]
		}
	}
	v, _ := protowire.ConsumeVarint(first(m, 5))
	k.visible = v == 1
	return k
}
func rpc(ctx context.Context, c *grpc.ClientConn, method string, req []byte) ([]byte, error) {
	var out []byte
	err := c.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, &req, &out, grpc.ForceCodec(wireCodec{}))
	return out, err
}
func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func TestLifecycle(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--auth.api_key_group_cache_ttl=1s"}, harness.CacheProxy: {"--auth.remote.jwt_cache_ttl=1s"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	admin := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "lifecycle-admin", []string{"ORG_ADMIN", "CACHE_WRITE"}))
	rc := msg(1, str(2, f.GroupID))
	webRC := f.Web.RequestContext(f.GroupID)
	call := func(method string, b []byte) []byte {
		t.Helper()
		out, err := rpc(ctx, admin.Conn, method, append(append([]byte(nil), rc...), b...))
		require(t, err)
		return out
	}
	var model key
	check := func(present bool) {
		t.Helper()
		for _, method := range []string{"GetApiKeys", "GetBazelConfig"} {
			out := call(method, nil)
			n := protowire.Number(2)
			if method == "GetBazelConfig" {
				n = 4
			}
			var got *key
			for _, b := range fields(out)[n] {
				if n == 4 {
					b = first(fields(b), 1)
				}
				k := decode(b)
				if k.id == model.id {
					got = &k
				}
			}
			if (got != nil) != present {
				t.Fatalf("%s presence=%v want %v", method, got != nil, present)
			}
			if got != nil {
				if got.value != "" || got.label != model.label || (method == "GetApiKeys" && !reflect.DeepEqual(got.caps, model.caps)) || got.visible != model.visible {
					t.Fatalf("%s metadata mismatch: %+v model %+v", method, *got, model)
				}
			}
			res, err := f.Web.RPC(method, map[string]any{"requestContext": webRC})
			require(t, err)
			field := "apiKey"
			if method == "GetBazelConfig" {
				field = "credential"
			}
			found := false
			if arr, ok := res[field].([]any); ok {
				for _, v := range arr {
					m := v.(map[string]any)
					if field == "credential" {
						m = m["apiKey"].(map[string]any)
					}
					if m["id"] == model.id {
						found = true
						projected := model
						if method == "GetBazelConfig" {
							projected.caps = nil
						}
						checkJSON(t, m, projected, false)
					}
				}
			}
			if found != present {
				t.Fatalf("HTTP %s presence=%v want %v", method, found, present)
			}
		}
		out, err := rpc(ctx, admin.Conn, "GetApiKey", append(append([]byte(nil), rc...), str(2, model.id)...))
		res, we := f.Web.RPC("GetApiKey", map[string]any{"requestContext": webRC, "apiKeyId": model.id})
		if !present {
			if status.Code(err) != codes.NotFound || we == nil {
				t.Fatalf("deleted detail: grpc=%v HTTP=%v", err, we)
			}
			return
		}
		require(t, err)
		require(t, we)
		got := decode(first(fields(out), 2))
		if !reflect.DeepEqual(got, model) {
			t.Fatalf("detail mismatch %+v model %+v", got, model)
		}
		checkJSON(t, res["apiKey"].(map[string]any), model, true)
	}
	// Repeated creation is valid but intentionally creates distinct identities.
	for i := 0; i < 2; i++ {
		b := append(str(3, "created"), num(4, 1)...)
		model = decode(first(fields(call("CreateApiKey", b)), 2))
		if model.id == "" || model.value == "" {
			t.Fatal("missing created identity")
		}
		check(true)
		clients := []*harness.Clients{harness.Dial(t, f.Endpoint(harness.App).GRPC, model.value), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, model.value)}
		auth := func(capability uint64, live bool) error {
			req := []byte{}
			var out []byte
			err := clients[0].Conn.Invoke(ctx, "/auth.AuthService/Authenticate", &req, &out, grpc.ForceCodec(wireCodec{}))
			if !live {
				if err == nil {
					return fmt.Errorf("revoked key still authenticates")
				}
			} else {
				if err != nil {
					return err
				}
				jwt := string(first(fields(out), 1))
				parts := strings.Split(jwt, ".")
				if len(parts) != 3 {
					return fmt.Errorf("missing JWT")
				}
				payload, e := base64.RawURLEncoding.DecodeString(parts[1])
				if e != nil {
					return e
				}
				var claims map[string]any
				if e = json.Unmarshal(payload, &claims); e != nil {
					return e
				}
				caps, _ := claims["capabilities"].([]any)
				if len(caps) != 1 || caps[0] != float64(capability) || claims["group_id"] != f.GroupID {
					return fmt.Errorf("JWT capabilities %v want %d", claims, capability)
				}
			}
			for _, c := range clients {
				_, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{harness.Digest([]byte("probe"))}})
				if live && err != nil {
					return err
				}
				if !live && status.Code(err) != codes.Unauthenticated {
					return fmt.Errorf("revoked CAS auth: %v", err)
				}
			}
			return nil
		}
		eventually(t, ctx, func() error { return auth(1, true) })
		for _, step := range []struct {
			label   string
			cap     uint64
			visible bool
		}{{"updated", 4, true}, {"updated", 4, true}, {"", 1, false}} {
			b := append(str(2, model.id), str(3, step.label)...)
			b = append(b, num(4, step.cap)...)
			if step.visible {
				b = append(b, num(5, 1)...)
			}
			call("UpdateApiKey", b)
			model.label = step.label
			model.caps = []uint64{step.cap}
			model.visible = step.visible
			check(true)
			eventually(t, ctx, func() error { return auth(step.cap, true) })
		}
		call("DeleteApiKey", str(2, model.id))
		check(false)
		eventually(t, ctx, func() error { return auth(0, false) })
	}
}
func eventually(t *testing.T, ctx context.Context, fn func() error) {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		err := fn()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
			t.Fatalf("authentication did not converge: %v", err)
		case <-tick.C:
		}
	}
}
func checkJSON(t *testing.T, m map[string]any, want key, secret bool) {
	t.Helper()
	label, _ := m["label"].(string)
	value, _ := m["value"].(string)
	visible, _ := m["visibleToDevelopers"].(bool)
	if label != want.label || visible != want.visible || (secret && value != want.value) || (!secret && value != "") {
		t.Fatalf("HTTP metadata mismatch for %s", want.id)
	}
	names := map[uint64]string{1: "CACHE_WRITE", 4: "CAS_WRITE"}
	caps, _ := m["capability"].([]any)
	if len(caps) != len(want.caps) {
		t.Fatalf("HTTP capability count mismatch")
	}
	for i, v := range caps {
		if v != names[want.caps[i]] && v != float64(want.caps[i]) {
			t.Fatalf("HTTP capability %v", v)
		}
	}
}
