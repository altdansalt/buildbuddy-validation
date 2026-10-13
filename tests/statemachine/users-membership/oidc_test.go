package membership

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

// A test-owned OIDC issuer. Every login gets its own authorization code and
// identity; no shared deployment or host credential participates in login.
type issuer struct {
	url   string
	key   *rsa.PrivateKey
	mu    sync.Mutex
	codes map[string]string
	next  int
}

func startIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	// UDP connect selects the host interface without sending any packet. The
	// documentation-only destination is never used by the HTTP/OIDC fixture.
	route, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	hostIP := route.LocalAddr().(*net.UDPAddr).IP.String()
	route.Close()
	p := &issuer{url: fmt.Sprintf("http://%s:%d", hostIP, ln.Addr().(*net.TCPAddr).Port), key: key, codes: map[string]string{}}
	s := &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 5 * time.Second}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	go s.Serve(ln)
	return p
}
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func (p *issuer) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		json.NewEncoder(w).Encode(map[string]any{"issuer": p.url, "authorization_endpoint": p.url + "/authorize", "token_endpoint": p.url + "/token", "jwks_uri": p.url + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	case "/keys":
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "fixture", "use": "sig", "alg": "RS256", "n": b64(p.key.N.Bytes()), "e": b64([]byte{1, 0, 1})}}})
	case "/authorize":
		// Browser-only identity selection is carried in a test cookie on the issuer.
		c, err := r.Cookie("identity")
		if err != nil {
			http.Error(w, "missing identity", 400)
			return
		}
		p.mu.Lock()
		p.next++
		code := strconv.Itoa(p.next)
		p.codes[code] = c.Value
		p.mu.Unlock()
		u, err := url.Parse(r.URL.Query().Get("redirect_uri"))
		if err != nil {
			http.Error(w, "redirect", 400)
			return
		}
		q := u.Query()
		q.Set("code", code)
		q.Set("state", r.URL.Query().Get("state"))
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	case "/token":
		r.ParseForm()
		p.mu.Lock()
		id, ok := p.codes[r.Form.Get("code")]
		delete(p.codes, r.Form.Get("code"))
		p.mu.Unlock()
		if !ok {
			http.Error(w, "unknown code", 400)
			return
		}
		header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "fixture"})
		claims, _ := json.Marshal(map[string]any{"iss": p.url, "sub": id, "aud": "membership-client", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "email": id + "@membership.test", "email_verified": true, "given_name": id, "family_name": "Fixture", "name": id + " Fixture"})
		unsigned := b64(header) + "." + b64(claims)
		hash := sha256.Sum256([]byte(unsigned))
		sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, hash[:])
		if err != nil {
			http.Error(w, "sign", 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-" + id, "token_type": "Bearer", "expires_in": 3600, "id_token": unsigned + "." + b64(sig)})
	default:
		http.NotFound(w, r)
	}
}
func login(t *testing.T, f *harness.Fixture, p *issuer, id string) *harness.WebSession {
	t.Helper()
	ep := f.Endpoint(harness.App)
	app, _ := url.Parse(ep.SelfAuthURL)
	published, _ := url.Parse(ep.HTTP)
	oidc, _ := url.Parse(p.url)
	jar, _ := cookiejar.New(nil)
	jar.SetCookies(oidc, []*http.Cookie{{Name: "identity", Value: id, Path: "/"}})
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch addr {
		case app.Host:
			addr = published.Host
		case oidc.Host:
			addr = "127.0.0.1:" + oidc.Port()
		default:
			return nil, fmt.Errorf("request outside fixture: %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	w := &harness.WebSession{URL: ep.SelfAuthURL, Client: &http.Client{Transport: tr, Jar: jar, Timeout: 15 * time.Second}}
	res, err := w.Client.Get(w.URL + "/login/?issuer_url=" + url.QueryEscape(p.url) + "&redirect_url=" + url.QueryEscape(w.URL))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	found := false
	for _, c := range jar.Cookies(app) {
		if c.Name == "Authorization" && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("OIDC %s login failed: HTTP %d final URL %s", id, res.StatusCode, res.Request.URL)
	}
	return w
}
