package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"
	"time"
)

// WebSession uses the app's real built-in self-OIDC login and JSON protolet API.
// This provisions test-local organizations and keys without touching SQLite.
// Self-auth has a fixed single identity. Use a local mock OIDC for multiple users.
type WebSession struct {
	Client  *http.Client
	URL     string
	UserID  map[string]any
	GroupID string
}

func LoginSelfAuth(t testing.TB, ep *Endpoint) *WebSession {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := url.Parse(ep.SelfAuthURL)
	if err != nil {
		t.Fatal(err)
	}
	published, err := url.Parse(ep.HTTP)
	if err != nil {
		t.Fatal(err)
	}
	// The app can resolve its own Docker alias, while this process uses the
	// published port. Keep the OIDC issuer and cookie origin identical on both.
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != issuer.Host {
			return nil, fmt.Errorf("self-auth request outside fixture: %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, published.Host)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	w := &WebSession{Client: &http.Client{Jar: jar, Transport: transport, Timeout: 30 * time.Second}, URL: ep.SelfAuthURL}
	resp, err := w.Client.Get(w.URL + "/login/?issuer_url=" + url.QueryEscape(w.URL) + "&redirect_url=" + url.QueryEscape(w.URL))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	origin, _ := url.Parse(w.URL)
	loggedIn := false
	for _, c := range jar.Cookies(origin) {
		if c.Name == "Authorization" && c.Value != "" {
			loggedIn = true
		}
	}
	if !loggedIn {
		t.Fatalf("self-auth did not set Authorization cookie (HTTP %d)", resp.StatusCode)
	}
	user, err := w.RPC("GetUser", map[string]any{})
	if err != nil {
		if _, err = w.RPC("CreateUser", map[string]any{}); err != nil {
			t.Fatal(err)
		}
		user, err = w.RPC("GetUser", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
	}
	display, _ := user["displayUser"].(map[string]any)
	w.UserID, _ = display["userId"].(map[string]any)
	w.GroupID, _ = user["selectedGroupId"].(string)
	if selected, ok := user["selectedGroup"].(map[string]any); ok {
		if g, ok := selected["groupId"].(string); ok {
			w.GroupID = g
		}
	}
	if w.GroupID == "" {
		t.Fatalf("self-auth user has no group: %v", user)
	}
	return w
}

func (w *WebSession) RPC(method string, req map[string]any) (map[string]any, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequest("POST", w.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := w.Client.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s HTTP %d: %s", method, resp.StatusCode, b)
	}
	var result map[string]any
	err = json.Unmarshal(b, &result)
	return result, err
}
func (w *WebSession) RequestContext(group string) map[string]any {
	return map[string]any{"userId": w.UserID, "groupId": group}
}
func (w *WebSession) CreateAPIKey(t testing.TB, group, label string, capabilities []string) string {
	t.Helper()
	res, err := w.RPC("CreateApiKey", map[string]any{"requestContext": w.RequestContext(group), "label": label, "capability": capabilities})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := res["apiKey"].(map[string]any)
	value, _ := key["value"].(string)
	if value == "" {
		t.Fatalf("empty API key response: %v", res)
	}
	return value
}

// CreateGroup makes an additional organization owned by the self-auth user.
func (w *WebSession) CreateGroup(t testing.TB, name string) string {
	t.Helper()
	res, err := w.RPC("CreateGroup", map[string]any{"requestContext": w.RequestContext(w.GroupID), "name": name})
	if err != nil {
		t.Fatal(err)
	}
	group, _ := res["id"].(string)
	if group == "" {
		t.Fatalf("empty group ID: %v", res)
	}
	return group
}
