package membership

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Isolate proxy JWT caching from the app's independent five-minute key cache.
func TestProxyMembershipRevocation(t *testing.T) { runProxyMembershipRevocation(t, false) }

func runProxyMembershipRevocation(t *testing.T, immediate bool) {
	p := startIssuer(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, SkipAuthBootstrap: true,
		Args:   map[harness.Component][]string{harness.App: {"--auth.api_key_group_cache_ttl=0"}},
		Config: map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_self_auth": false, "disable_refresh_token": true, "oauth_providers": []any{harness.Config{"issuer_url": p.url, "client_id": "membership-client", "client_secret": "fixture-secret"}}}, "app": harness.Config{"create_group_per_user": false, "user_owned_keys_enabled": true}}}})
	admin := createIdentity(t, f, p, "admin")
	member := createIdentity(t, f, p, "member")
	gid := stringValue(mustHTTP(t, admin.web, "CreateGroup", map[string]any{"requestContext": admin.web.RequestContext(admin.home), "name": "Membership target", "urlIdentifier": "membership-target", "userOwnedKeysEnabled": true})["id"])
	admin.manageKey = stringValue(object(mustHTTP(t, admin.web, "CreateApiKey", map[string]any{"requestContext": admin.web.RequestContext(gid), "label": "target admin", "capability": []string{"ORG_ADMIN"}})["apiKey"])["value"])
	conn := f.Clients(harness.App).Conn
	_, err := rpc(conn, admin.manageKey, "UpdateGroupUsers", join(bytesField(1, requestContext(admin.uid, gid)), text(2, gid), bytesField(3, join(bytesField(1, text(1, member.uid)), number(2, 1), number(3, 4)))))
	if err != nil {
		t.Fatal(err)
	}
	check(t, conn, admin, member, gid, model{1, 4})
	createdKey := object(mustHTTP(t, member.web, "CreateUserApiKey", map[string]any{"requestContext": member.web.RequestContext(gid), "userId": member.uid, "label": "revocation probe", "capability": []string{"CACHE_WRITE", "CAS_WRITE"}})["apiKey"])
	key := stringValue(createdKey["value"])
	keyID := stringValue(createdKey["id"])
	assertKeySurfaces(t, conn, admin, member, gid, keyID, true)
	assertAuthenticate(t, conn, key, true)
	if key == "" {
		t.Fatal("missing target user key")
	}
	app := harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
	proxy := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	payload := []byte("membership revocation probe")
	digest, err := app.Upload(ctx, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	data, err := proxy.Read(ctx, "", digest)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("warm proxy read: %q %v", data, err)
	}
	_, err = rpc(conn, admin.manageKey, "UpdateGroupUsers", join(bytesField(1, requestContext(admin.uid, gid)), text(2, gid), bytesField(3, join(bytesField(1, text(1, member.uid)), number(2, 2)))))
	if err != nil {
		t.Fatal(err)
	}
	check(t, conn, admin, member, gid, model{})
	assertKeySurfaces(t, conn, admin, member, gid, keyID, false)
	assertAuthenticate(t, conn, key, false)
	_, appErr := app.Read(ctx, "", digest)
	if status.Code(appErr) != codes.Unauthenticated && status.Code(appErr) != codes.PermissionDenied {
		t.Fatalf("removed key app read: %v", appErr)
	}
	data, proxyErr := proxy.Read(ctx, "", digest)
	t.Logf("post-removal app=%v proxy=%v proxy_data=%q", appErr, proxyErr, data)
	diverged := proxyErr == nil
	if proxyErr != nil && status.Code(proxyErr) != codes.Unauthenticated && status.Code(proxyErr) != codes.PermissionDenied {
		t.Fatal(proxyErr)
	}
	if proxyErr == nil && string(data) != string(payload) {
		t.Fatal("incorrect proxy payload")
	}
	began := time.Now()
	deadline := began.Add(75 * time.Second)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for proxyErr == nil && time.Now().Before(deadline) {
		<-ticker.C
		_, proxyErr = proxy.Read(ctx, "", digest)
	}
	if status.Code(proxyErr) != codes.Unauthenticated && status.Code(proxyErr) != codes.PermissionDenied {
		t.Fatalf("proxy failed to converge: %v", proxyErr)
	}
	convergedAfter := time.Since(began)
	for i := 0; i < 3; i++ {
		<-ticker.C
		_, err := proxy.Read(ctx, "", digest)
		if status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
			t.Fatalf("revocation not stable: %v", err)
		}
	}
	t.Logf("proxy rejected removed key after %s; app=%s proxy=%s", convergedAfter, f.Endpoint(harness.App).Image, f.Endpoint(harness.CacheProxy).Image)
	if immediate && diverged {
		t.Fatalf("membership revocation is visible in app but proxy served cached payload; converged after %s", convergedAfter)
	}
}

func assertAuthenticate(t *testing.T, conn *grpc.ClientConn, key string, allowed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-api-key", key)
	var out []byte
	err := conn.Invoke(ctx, "/auth.AuthService/Authenticate", []byte{}, &out, grpc.ForceCodec(wireCodec{}))
	if allowed {
		if err != nil {
			t.Fatal(err)
		}
		f, err := decode(out)
		if err != nil || len(first(f, 1)) == 0 {
			t.Fatalf("missing JWT: %v", err)
		}
	} else if status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Authenticate removed key: %v", err)
	}
}

func assertKeySurfaces(t *testing.T, conn *grpc.ClientConn, admin, member *identity, gid, keyID string, present bool) {
	t.Helper()
	h, httpErr := admin.web.RPC("GetUserApiKeys", map[string]any{"requestContext": admin.web.RequestContext(gid), "userId": member.uid})
	g, err := rpc(conn, admin.manageKey, "GetUserApiKeys", join(bytesField(1, requestContext(admin.uid, gid)), text(3, member.uid)))
	if !present {
		if status.Code(err) != codes.PermissionDenied || httpErr == nil || !strings.Contains(httpErr.Error(), "PermissionDenied") {
			t.Fatalf("removed user key list HTTP=%v gRPC=%v", httpErr, err)
		}
		return
	}
	if err != nil || httpErr != nil {
		t.Fatalf("key list HTTP=%v gRPC=%v", httpErr, err)
	}
	want := 0
	if present {
		want = 1
	}
	if len(array(h["apiKey"])) != want || len(g[2]) != want {
		t.Fatalf("key list HTTP=%v gRPC=%v want %d", h, g, want)
	}
	if !present {
		return
	}
	if object(array(h["apiKey"])[0])["id"] != keyID {
		t.Fatal("HTTP wrong key")
	}
	k, err := decode(first(g, 2))
	if err != nil || string(first(k, 1)) != keyID {
		t.Fatal("gRPC wrong key")
	}
	h = mustHTTP(t, admin.web, "GetUserApiKey", map[string]any{"requestContext": admin.web.RequestContext(gid), "apiKeyId": keyID})
	g, err = rpc(conn, admin.manageKey, "GetUserApiKey", join(bytesField(1, requestContext(admin.uid, gid)), text(2, keyID)))
	if err != nil {
		t.Fatal(err)
	}
	k, err = decode(first(g, 2))
	if err != nil || string(first(k, 1)) != keyID || object(h["apiKey"])["id"] != keyID {
		t.Fatal("key detail differs")
	}
}
