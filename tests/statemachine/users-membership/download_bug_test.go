package membership

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func httpDownload(ctx context.Context, base, key, rn string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/file/download?bytestream_url="+url.QueryEscape(rn), nil)
	if err != nil {
		return 0, "", err
	}
	if key != "" {
		req.Header.Set("x-buildbuddy-api-key", key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, string(b), err
}

// TestRemovedMemberHTTPDownload checks that /file/download rejects a removed
// member's user-owned key, like every gRPC cache read does. Controls with a
// bogus key and no credentials show whether the endpoint enforces auth at all.
func TestRemovedMemberHTTPDownload(t *testing.T) { runRemovedMemberHTTPDownload(t, false) }

// Same sequence with the harness's anonymous-usage default switched off, to
// show whether the divergence depends on that non-release-default fixture setting.
func TestRemovedMemberHTTPDownloadNoAnonymous(t *testing.T) { runRemovedMemberHTTPDownload(t, true) }

func runRemovedMemberHTTPDownload(t *testing.T, noAnonymous bool) {
	p := startIssuer(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, SkipAuthBootstrap: true,
		Args:   map[harness.Component][]string{harness.App: {"--auth.api_key_group_cache_ttl=0"}},
		Config: map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_anonymous_usage": !noAnonymous, "enable_self_auth": false, "disable_refresh_token": true, "oauth_providers": []any{harness.Config{"issuer_url": p.url, "client_id": "membership-client", "client_secret": "fixture-secret"}}}, "app": harness.Config{"create_group_per_user": false, "user_owned_keys_enabled": true}}}})
	admin := createIdentity(t, f, p, "admin")
	member := createIdentity(t, f, p, "member")
	gid := stringValue(mustHTTP(t, admin.web, "CreateGroup", map[string]any{"requestContext": admin.web.RequestContext(admin.home), "name": "Membership target", "urlIdentifier": "membership-target", "userOwnedKeysEnabled": true})["id"])
	admin.manageKey = stringValue(object(mustHTTP(t, admin.web, "CreateApiKey", map[string]any{"requestContext": admin.web.RequestContext(gid), "label": "target admin", "capability": []string{"ORG_ADMIN", "CACHE_WRITE", "CAS_WRITE"}})["apiKey"])["value"])
	conn := f.Clients(harness.App).Conn
	update := func(action uint64) {
		t.Helper()
		u := join(bytesField(1, text(1, member.uid)), number(2, action))
		if action == 1 {
			u = append(u, number(3, 4)...)
		}
		if _, err := rpc(conn, admin.manageKey, "UpdateGroupUsers", join(bytesField(1, requestContext(admin.uid, gid)), text(2, gid), bytesField(3, u))); err != nil {
			t.Fatal(err)
		}
	}
	update(1)
	key := stringValue(object(mustHTTP(t, member.web, "CreateUserApiKey", map[string]any{"requestContext": member.web.RequestContext(gid), "userId": member.uid, "label": "download", "capability": []string{"CACHE_WRITE", "CAS_WRITE"}})["apiKey"])["value"])
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	payload := []byte("membership http download probe")
	d, err := harness.Dial(t, f.Endpoint(harness.App).GRPC, admin.manageKey).Upload(ctx, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	rn := fmt.Sprintf("bytestream://app:1985/blobs/%s/%d", d.Hash, d.SizeBytes)
	base := f.Endpoint(harness.App).HTTP
	if code, body, err := httpDownload(ctx, base, key, rn); err != nil || code != 200 || body != string(payload) {
		t.Fatalf("member download: %d %q %v", code, body, err)
	}
	update(2)
	member2 := harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
	_, grpcErr := member2.Read(ctx, "", d)
	if status.Code(grpcErr) != codes.Unauthenticated && status.Code(grpcErr) != codes.PermissionDenied {
		t.Fatalf("gRPC read by removed member: %v", grpcErr)
	}
	_, anonErr := harness.Dial(t, f.Endpoint(harness.App).GRPC, "").Read(ctx, "", d)
	t.Logf("control: unauthenticated gRPC read of the same blob: %v", anonErr)
	for name, k := range map[string]string{"removed key": key, "bogus key": "bogus-key-0123456789ab", "no credentials": ""} {
		code, body, err := httpDownload(ctx, base, k, rn)
		t.Logf("%s: HTTP %d %q err=%v (gRPC removed-key read: %v)", name, code, body, err, grpcErr)
	}
	deadline := time.Now().Add(45 * time.Second)
	var code int
	var body string
	for {
		code, body, err = httpDownload(ctx, base, key, rn)
		if err != nil {
			t.Fatal(err)
		}
		if code != 200 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if code == 200 {
		t.Fatalf("anonymousDisabled=%v: removed member still downloads the blob over HTTP after 45s: %q (image %s)", noAnonymous, body, f.Endpoint(harness.App).Image)
	}
}
