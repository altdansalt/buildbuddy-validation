package userkeys

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"
	"testing"
	"time"
)

// Independently exercise released auth-cache defaults, rather than extrapolating
// from the fast state machine's one-second caches.
func TestDefaultTTLRevocation(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"user_owned_keys_enabled": true}}}})
	_, e := f.Web.RPC("UpdateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "id": f.GroupID, "name": "default-ttl", "urlIdentifier": "default-ttl", "userOwnedKeysEnabled": true})
	must(t, e)
	response, e := f.Web.RPC("CreateUserApiKey", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "userId": f.Web.UserID["id"], "label": "default-revocation", "capability": []string{"CACHE_WRITE"}})
	must(t, e)
	k := fromJSON(response["apiKey"].(map[string]any))
	clients := map[harness.Component]*harness.Clients{}
	probe := func(c *harness.Clients) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{harness.Digest([]byte("default-auth"))}})
		return e
	}
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		clients[component] = harness.Dial(t, f.Endpoint(component).GRPC, k.Value)
		must(t, probe(clients[component]))
	}
	_, e = f.Web.RPC("DeleteUserApiKey", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "id": k.ID})
	must(t, e)
	started := time.Now()
	for _, method := range []string{"GetApiKey", "GetUserApiKey"} {
		_, e = f.Web.RPC(method, map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "apiKeyId": k.ID})
		if e == nil || !strings.Contains(e.Error(), "NotFound") {
			t.Fatalf("%s deletion: %v", method, e)
		}
	}
	stable := map[harness.Component]int{}
	first := map[harness.Component]time.Duration{}
	for {
		all := true
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			e := probe(clients[component])
			if status.Code(e) == codes.Unauthenticated || status.Code(e) == codes.PermissionDenied {
				if stable[component] == 0 {
					first[component] = time.Since(started)
				}
				stable[component]++
			} else {
				stable[component] = 0
			}
			if stable[component] < 3 {
				all = false
			}
		}
		if all {
			break
		}
		if time.Since(started) > 7*time.Minute {
			t.Fatalf("default revocation did not converge: %v", stable)
		}
		time.Sleep(time.Second)
	}
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Logf("default %s revocation first=%s stable=%s", component, first[component], time.Since(started))
	}
	for _, method := range []string{ui + "GetUser", "/auth.AuthService/Authenticate"} {
		_, e = rpc(clients[harness.App], method, nil)
		if status.Code(e) != codes.Unauthenticated && status.Code(e) != codes.PermissionDenied {
			t.Fatalf("revoked %s: %v", method, e)
		}
	}
}

// Expiry is a state transition without a delete RPC. Read projections must
// stop offering an expired credential, and both auth caches must reject it.
func TestExpiry(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"user_owned_keys_enabled": true}, "auth": harness.Config{"api_key_group_cache_ttl": "1s"}}, harness.CacheProxy: {"auth": harness.Config{"remote": harness.Config{"jwt_cache_ttl": "1s"}}}}})
	rc := f.Web.RequestContext(f.GroupID)
	_, e := f.Web.RPC("UpdateGroup", map[string]any{"requestContext": rc, "id": f.GroupID, "name": "expiry", "urlIdentifier": "expiry", "userOwnedKeysEnabled": true})
	must(t, e)
	r, e := f.Web.RPC("CreateUserApiKey", map[string]any{"requestContext": rc, "userId": f.Web.UserID["id"], "label": "expires", "capability": []string{"CACHE_WRITE"}, "expiresIn": "5s"})
	must(t, e)
	k := fromJSON(r["apiKey"].(map[string]any))
	adminKey := f.Web.CreateAPIKey(t, f.GroupID, "expiry-admin", []string{"ORG_ADMIN", "CACHE_WRITE"})
	admin := harness.Dial(t, f.Endpoint(harness.App).GRPC, adminKey)
	base := str(nil, 1, string(str(nil, 2, f.GroupID)))
	for _, method := range []string{"GetApiKey", "GetUserApiKey"} {
		b, e := rpc(admin, ui+method, str(base, 2, k.ID))
		must(t, e)
		compare(t, k, fromWire(parse(b)[2][0]), true)
		w, e := f.Web.RPC(method, map[string]any{"requestContext": rc, "apiKeyId": k.ID})
		must(t, e)
		compare(t, k, fromJSON(w["apiKey"].(map[string]any)), true)
	}
	clients := []*harness.Clients{harness.Dial(t, f.Endpoint(harness.App).GRPC, k.Value), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, k.Value)}
	started := time.Now()
	stable := 0
	for {
		rejected := true
		for _, c := range clients {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{})
			cancel()
			if status.Code(e) != codes.Unauthenticated && status.Code(e) != codes.PermissionDenied {
				rejected = false
			}
		}
		if rejected {
			stable++
		} else {
			stable = 0
		}
		if stable >= 3 {
			break
		}
		if time.Since(started) > 20*time.Second {
			t.Fatal("expiry did not converge")
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, method := range []string{"GetApiKey", "GetUserApiKey"} {
		_, e := rpc(admin, ui+method, str(base, 2, k.ID))
		if status.Code(e) != codes.NotFound {
			t.Fatalf("expired %s grpc: %v", method, e)
		}
		_, e = f.Web.RPC(method, map[string]any{"requestContext": rc, "apiKeyId": k.ID})
		if e == nil || !strings.Contains(e.Error(), "NotFound") {
			t.Fatalf("expired %s HTTP: %v", method, e)
		}
	}
	b, e := rpc(admin, ui+"GetUserApiKeys", str(base, 3, f.Web.UserID["id"].(string)))
	must(t, e)
	if len(parse(b)[2]) != 0 {
		t.Fatal("expired key listed")
	}
	w, e := f.Web.RPC("GetUserApiKeys", map[string]any{"requestContext": rc, "userId": f.Web.UserID["id"]})
	must(t, e)
	if a, ok := w["apiKey"].([]any); ok && len(a) != 0 {
		t.Fatal("expired key HTTP listed")
	}
	w, e = f.Web.RPC("GetBazelConfig", map[string]any{"requestContext": rc, "host": "app", "protocol": "http:"})
	must(t, e)
	if a, ok := w["credential"].([]any); ok {
		for _, v := range a {
			if fromJSON(v.(map[string]any)["apiKey"].(map[string]any)).ID == k.ID {
				t.Fatal("expired credential offered in setup")
			}
		}
	}
	t.Logf("expiry stable rejection observed after %s (one-second auth caches)", time.Since(started))
}
