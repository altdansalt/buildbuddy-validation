package githubstate

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CONNECT is a closed routing table: even hard-coded public GitHub URLs terminate
// at this test's TLS server. No connection is ever made to the requested host.
func githubMock(t *testing.T) (proxyURL, certPath, privateKey string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture GitHub"}, DNSNames: []string{"github.com", "api.github.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	must(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	must(t, err)
	installation := map[string]any{"id": int64(101), "app_id": int64(7), "account": map[string]any{"login": "fixture-owner"}, "target_type": "Organization", "html_url": "https://github.com/settings/installations/101", "permissions": map[string]string{"administration": "read"}}
	repo := map[string]any{"name": "project", "full_name": "fixture-owner/project", "clone_url": repoURL, "html_url": repoURL, "default_branch": "main"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/login/oauth/access_token" && r.URL.Path != "/fixture-content" && !strings.HasPrefix(r.URL.Path, "/applications/") {
			auth := r.Header.Get("Authorization")
			if auth == "" {
				t.Errorf("missing authorization on %s", r.URL.Path)
				http.Error(w, "missing auth", 401)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/user") || r.URL.Path == "/graphql" || strings.Contains(r.URL.Path, "/git/") || strings.HasSuffix(r.URL.Path, "/commits") || strings.HasSuffix(r.URL.Path, "/forks") || strings.Contains(r.URL.Path, "/compare/") || strings.Contains(r.URL.Path, "/contents/") {
				if !strings.Contains(auth, "fixture-only-token") {
					t.Errorf("wrong user token on %s: %s", r.URL.Path, auth)
					http.Error(w, "wrong auth", 401)
					return
				}
			}
		}
		var result any
		switch {
		case r.URL.Path == "/login/oauth/access_token":
			result = map[string]string{"access_token": "fixture-only-token", "token_type": "bearer"}
		case r.URL.Path == "/app/installations/101/access_tokens":
			result = map[string]string{"token": "fixture-installation-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}
		case r.URL.Path == "/repos/fixture-owner/project/contents/":
			result = []any{map[string]any{"type": "file", "name": "README.md", "path": "README.md", "download_url": "https://api.github.com/fixture-content"}}
		case r.URL.Path == "/repos/fixture-owner/project/contents/README.md":
			result = map[string]any{"type": "file", "name": "README.md", "path": "README.md", "download_url": "https://api.github.com/fixture-content"}
		case r.URL.Path == "/fixture-content" || r.URL.Path == "/repos/fixture-owner/project/git/blobs/abc":
			w.Write([]byte("fixture content\n"))
			return
		case r.URL.Path == "/repos/fixture-owner/project/git/trees/main":
			result = map[string]any{"sha": "tree-sha", "tree": []any{map[string]any{"path": "README.md", "sha": "abc", "type": "blob", "mode": "100644", "size": 16}}}
		case r.URL.Path == "/repos/fixture-owner/project/compare/main...topic":
			result = map[string]any{"ahead_by": 1, "commits": []any{}, "files": []any{}}
		case r.URL.Path == "/repos/fixture-owner/project/forks":
			result = []any{map[string]any{"owner": map[string]any{"login": "fixture-fork"}}}
		case r.URL.Path == "/repos/fixture-owner/project/commits":
			result = []any{map[string]any{"sha": "commit-sha", "commit": map[string]any{"message": "fixture commit", "tree": map[string]any{"sha": "tree-sha"}}}}
		case r.URL.Path == "/repos/fixture-owner/project/pulls/1/files":
			result = []any{}
		case r.URL.Path == "/graphql":
			var query struct {
				Query string `json:"query"`
			}
			must(t, json.NewDecoder(r.Body).Decode(&query))
			if strings.Contains(query.Query, "search(") {
				result = map[string]any{"data": map[string]any{"viewer": map[string]any{"login": "fixture-user"}, "search": map[string]any{"nodes": []any{}}}}
			} else {
				result = map[string]any{"data": map[string]any{"viewer": map[string]any{"login": "fixture-user"}, "repository": map[string]any{"pullRequest": map[string]any{"id": "PR_fixture", "title": "Fixture PR", "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z", "mergeStateStatus": "CLEAN", "headRefName": "topic", "baseRefOid": "base", "headRefOid": "head", "author": map[string]any{"__typename": "User", "login": "fixture-user"}, "timelineItems": map[string]any{"nodes": []any{}}, "reviewRequests": map[string]any{"nodes": []any{}}, "reviewThreads": map[string]any{"nodes": []any{}}, "reviews": map[string]any{"nodes": []any{}}, "commits": map[string]any{"nodes": []any{}}}}}}
			}
		case r.URL.Path == "/repos/fixture-owner/project":
			result = repo
		case r.URL.Path == "/repos/fixture-owner/project/installation":
			result = installation
		case r.URL.Path == "/app/installations/101":
			result = installation
		case r.URL.Path == "/user/installations":
			result = map[string]any{"total_count": 1, "installations": []any{installation}}
		case r.URL.Path == "/user/installations/101/repositories":
			result = map[string]any{"total_count": 1, "repositories": []any{repo}}
		case r.URL.Path == "/user/repos":
			result = []any{repo}
		case r.URL.Path == "/user":
			result = map[string]string{"login": "fixture-user", "name": "Fixture User"}
		case strings.HasPrefix(r.URL.Path, "/applications/") && strings.HasSuffix(r.URL.Path, "/token"):
			result = map[string]string{"token": "fixture-only-token"}
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected mock route", 500)
			return
		}
		must(t, json.NewEncoder(w).Encode(result))
	})
	backend := httptest.NewUnstartedServer(handler)
	backend.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	backend.StartTLS()
	t.Cleanup(backend.Close)
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || (r.Host != "github.com:443" && r.Host != "api.github.com:443") {
			t.Logf("blocked non-GitHub initialization destination %s", r.Host)
			http.Error(w, "forbidden destination", 403)
			return
		}
		upstream, err := net.DialTimeout("tcp", backend.Listener.Addr().String(), 5*time.Second)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		client, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		fmt.Fprint(buf, "HTTP/1.1 200 Connection Established\r\n\r\n")
		buf.Flush()
		go func() { defer upstream.Close(); defer client.Close(); io.Copy(upstream, buf) }()
		io.Copy(client, upstream)
	}))
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	must(t, err)
	proxy.Listener = listener
	proxy.Start()
	t.Cleanup(proxy.Close)
	// A routable runner address works with both the harness-owned bridge-less
	// dockerd and the runner's prestarted daemon.
	addresses, err := net.InterfaceAddrs()
	must(t, err)
	ip := ""
	for _, a := range addresses {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			ip = n.IP.String()
			break
		}
	}
	if ip == "" {
		t.Fatal("runner has no routable IPv4 address")
	}
	certPath = filepath.Join(t.TempDir(), "github-ca.pem")
	must(t, os.WriteFile(certPath, certPEM, 0644))
	return "http://" + net.JoinHostPort(ip, fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)), certPath, string(keyPEM)
}
