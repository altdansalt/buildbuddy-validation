package sso_config

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

// These minimal wire bindings mirror context.proto and grp.proto at v2.314.0.
// Keeping them local avoids adding released-source dependencies to the scaffold.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func readField(t *testing.T, b []byte, want protowire.Number) []byte {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		b = b[k:]
		size := protowire.ConsumeFieldValue(n, typ, b)
		require.GreaterOrEqual(t, size, 0)
		if n == want {
			require.Equal(t, protowire.BytesType, typ)
			v, k := protowire.ConsumeBytes(b)
			require.GreaterOrEqual(t, k, 0)
			return v
		}
		b = b[size:]
	}
	return nil
}

func readBool(t *testing.T, b []byte, want protowire.Number) bool {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		b = b[k:]
		size := protowire.ConsumeFieldValue(n, typ, b)
		require.GreaterOrEqual(t, size, 0)
		if n == want {
			require.Equal(t, protowire.VarintType, typ)
			v, k := protowire.ConsumeVarint(b)
			require.Greater(t, k, 0)
			return v != 0
		}
		b = b[size:]
	}
	return false
}

var replaceProvider = flag.Bool("sso_replace", false, "reproduce stale login destination after replacement")

var immediate = flag.Bool("sso_immediate", false, "require immediate SAML invalidation")

func TestSSOStateMachine(t *testing.T) {
	// The app must validate real TLS, not skip certificate verification.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "metadata.fixture"}, DNSNames: []string{"metadata.fixture"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	certDir := t.TempDir()
	certPath := filepath.Join(certDir, "ca.pem")
	keyPath := filepath.Join(certDir, "key.pem")
	readyPath := filepath.Join(certDir, "ready")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0644))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600))
	opts := harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--http.client.allow_localhost=true", "--auth.saml.cert_file=/fixture-ca.pem", "--auth.saml.key_file=/fixture-key.pem"}}, DockerArgs: map[harness.Component][]string{harness.App: {"--volume", certPath + ":/fixture-ca.pem:ro", "--volume", keyPath + ":/fixture-key.pem:ro", "--env", "SSL_CERT_FILE=/fixture-ca.pem"}}}
	if !*immediate {
		opts.Config = map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"build_buddy_url": "http://app.fixture.test:8080", "enable_subdomain_matching": true, "default_subdomains": []string{"app"}}}}
		opts.DockerArgs[harness.App] = append(opts.DockerArgs[harness.App], "--network-alias", "app.fixture.test")
	}
	f := harness.Start(t, opts)
	groupSetup, err := f.Web.RPC("CreateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "name": "SSO conformance", "urlIdentifier": "sso-conformance"})
	require.NoError(t, err)
	groupID, ok := groupSetup["id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, groupID)
	otherGroup := f.GroupID
	f.GroupID = groupID

	if !*immediate {
		configPath := filepath.Join(f.OutputDir, "app.yaml")
		cfg, err := os.ReadFile(configPath)
		require.NoError(t, err)
		require.Contains(t, string(cfg), "auth:\n")
		cfg = []byte(strings.Replace(string(cfg), "auth:\n", "auth:\n    admin_group_id: "+groupID+"\n", 1))
		require.NoError(t, os.WriteFile(configPath, cfg, 0600))
		for _, args := range [][]string{{"cp", configPath, f.Endpoint(harness.App).Container + ":/config.yaml"}, {"restart", f.Endpoint(harness.App).Container}} {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
			cancel()
			require.NoError(t, err, "%s", out)
		}

		for _, port := range []string{"8080", "1985"} {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			out, err := exec.CommandContext(ctx, "docker", "port", f.Endpoint(harness.App).Container, port).CombinedOutput()
			cancel()
			require.NoError(t, err, "%s", out)
			address := strings.TrimSpace(string(out))
			if port == "8080" {
				f.Endpoint(harness.App).HTTP = "http://" + address
			} else {
				f.Endpoint(harness.App).GRPC = address
			}
		}
		require.Eventually(t, func() bool {
			resp, err := (&http.Client{Timeout: time.Second}).Get(f.Endpoint(harness.App).HTTP + "/readyz?server-type=app")
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}, 30*time.Second, time.Second)
		f.Web = harness.LoginSelfAuth(t, f.Endpoint(harness.App))
	}
	// The targeted invocation initializes Docker; inspect only our fixture's container.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Pid}}", f.Endpoint(harness.App).Container).CombinedOutput()
	cancel()
	require.NoError(t, err, "%s", out)
	pid := strings.TrimSpace(string(out))
	binary, err := os.Executable()
	require.NoError(t, err)
	helper := exec.Command("sudo", "-n", "nsenter", "--target", pid, "--net", "--", "env", "SSO_METADATA_HELPER="+certDir, binary, "-test.run=^TestMetadataHelper$", "-test.v")
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	log, err := os.Create(filepath.Join(f.OutputDir, "metadata.log"))
	require.NoError(t, err)
	helper.Stdout = log
	helper.Stderr = log
	require.NoError(t, helper.Start())
	t.Cleanup(func() {
		exec.Command("sudo", "-n", "kill", "-KILL", "--", fmt.Sprint(-helper.Process.Pid)).Run()
		helper.Wait()
		log.Close()
	})
	require.Eventually(t, func() bool { _, err := os.Stat(readyPath); return err == nil }, 10*time.Second, 50*time.Millisecond, "metadata server readiness")
	ready, err := os.ReadFile(readyPath)
	require.NoError(t, err)
	var base string
	require.NoError(t, json.Unmarshal(ready, &base))
	adminKey := f.Web.CreateAPIKey(t, f.GroupID, "sso-admin", []string{"ORG_ADMIN"})
	conn := harness.Dial(t, f.Endpoint(harness.App).GRPC, adminKey).Conn
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "sso-admin-two", []string{"ORG_ADMIN"})
	secondConn := harness.Dial(t, f.Endpoint(harness.App).GRPC, secondKey).Conn
	anonymous := harness.Dial(t, f.Endpoint(harness.App).GRPC, "").Conn
	anonymousWeb := &harness.WebSession{Client: &http.Client{Timeout: 10 * time.Second}, URL: f.Endpoint(harness.App).HTTP}
	requestContext := field(2, []byte(f.GroupID))
	invoke := func(method string, req []byte) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var response []byte
		require.NoError(t, conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, &req, &response, grpc.ForceCodec(wireCodec{})))
		return response
	}
	check := func(model string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req := field(1, requestContext)
		var rsp []byte
		err := secondConn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetSSOConfig", &req, &rsp, grpc.ForceCodec(wireCodec{}))
		cancel()
		require.NoError(t, err)
		require.Equal(t, model, string(readField(t, readField(t, rsp, 2), 1)))
		untouched, err := f.Web.RPC("GetSSOConfig", map[string]any{"requestContext": f.Web.RequestContext(otherGroup)})
		require.NoError(t, err)
		cfg, ok := untouched["config"].(map[string]any)
		require.True(t, ok)
		require.Empty(t, cfg["samlIdpMetadataUrl"], "other organization must remain unchanged")
		if !*immediate {
			r, err := http.NewRequest("POST", f.Endpoint(harness.App).HTTP+"/rpc/BuildBuddyService/GetGroup", strings.NewReader("{}"))
			require.NoError(t, err)
			r.Host = "sso-conformance.fixture.test"
			r.Header.Set("Content-Type", "application/json")
			response, err := anonymousWeb.Client.Do(r)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode)
			var subdomainGroup map[string]any
			err = json.NewDecoder(response.Body).Decode(&subdomainGroup)
			response.Body.Close()
			require.NoError(t, err)
			require.Equal(t, f.GroupID, subdomainGroup["id"])
			if v, exists := subdomainGroup["ssoEnabled"]; exists {
				require.IsType(t, false, v)
				require.Equal(t, model != "", v)
			} else {
				require.Empty(t, model)
			}
			byID := invoke("GetGroup", append(field(2, requestContext), field(3, []byte(f.GroupID))...))
			require.Equal(t, model != "", readBool(t, byID, 5), "server admin lookup by ID")
			adminWeb, err := f.Web.RPC("GetGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "groupId": f.GroupID})
			require.NoError(t, err)
			if v, exists := adminWeb["ssoEnabled"]; exists {
				require.IsType(t, false, v)
				require.Equal(t, model != "", v)
			} else {
				require.Empty(t, model)
			}
		}

		for _, c := range []*grpc.ClientConn{secondConn, anonymous} {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			req := field(1, []byte("sso-conformance"))
			var rsp []byte
			err := c.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetGroup", &req, &rsp, grpc.ForceCodec(wireCodec{}))
			cancel()
			require.NoError(t, err)
			require.Equal(t, model != "", readBool(t, rsp, 5))
		}
		public, err := anonymousWeb.RPC("GetGroup", map[string]any{"urlIdentifier": "sso-conformance"})
		require.NoError(t, err)
		if v, exists := public["ssoEnabled"]; exists {
			require.IsType(t, false, v)
			require.Equal(t, model != "", v)
		} else {
			require.Empty(t, model)
		}
		got := invoke("GetSSOConfig", field(1, requestContext))
		require.Equal(t, model, string(readField(t, readField(t, got, 2), 1)), "gRPC state")
		web, err := f.Web.RPC("GetSSOConfig", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		require.NoError(t, err)
		config, ok := web["config"].(map[string]any)
		require.True(t, ok, "%v", web)
		value := ""
		if v, exists := config["samlIdpMetadataUrl"]; exists {
			require.IsType(t, "", v)
			value = v.(string)
		}
		require.Equal(t, model, value, "UI protolet state")
		group := invoke("GetGroup", append(field(2, requestContext), field(1, []byte("sso-conformance"))...))
		require.Equal(t, model != "", readBool(t, group, 5), "gRPC public SSO enabled state")
		groupWeb, err := f.Web.RPC("GetGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "urlIdentifier": "sso-conformance"})
		require.NoError(t, err)
		enabled := false
		if v, exists := groupWeb["ssoEnabled"]; exists {
			require.IsType(t, false, v)
			enabled = v.(bool)
		}
		require.Equal(t, model != "", enabled, "UI public SSO enabled state")
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	saml := func(model string) bool {
		resp, err := client.Get(f.Endpoint(harness.App).HTTP + "/auth/saml/metadata?slug=sso-conformance")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		metadataStatus := resp.StatusCode
		metadataLocation := resp.Header.Get("Location")
		resp, err = client.Get(f.Endpoint(harness.App).HTTP + "/login/?slug=sso-conformance")
		require.NoError(t, err)
		resp.Body.Close()
		if *immediate {
			t.Logf("model=%q metadata=%d login=%d destination=%s", model, metadataStatus, resp.StatusCode, resp.Header.Get("Location"))
		}
		if model == "" {
			return metadataStatus == http.StatusTemporaryRedirect && strings.Contains(metadataLocation, "error=") && resp.StatusCode == http.StatusTemporaryRedirect && strings.Contains(resp.Header.Get("Location"), "error=")
		}
		require.Equal(t, http.StatusOK, metadataStatus)
		require.Contains(t, string(body), "EntityDescriptor")
		require.Equal(t, http.StatusFound, resp.StatusCode)
		return strings.Contains(resp.Header.Get("Location"), "https://metadata.fixture/"+strings.TrimPrefix(strings.TrimPrefix(model, base), "/")+"/login")
	}
	check("")
	require.True(t, saml(""))
	inputs := []string{" \t" + base + "/first.xml\n", base + "/second.xml", base + "/first.xml", "", base + "/second.xml", base + "/second.xml", "", ""}
	if *immediate {
		inputs = []string{base + "/first.xml", ""}
		if *replaceProvider {
			inputs[1] = base + "/second.xml"
		}
	}
	for i, input := range inputs {
		if i%2 == 0 {
			conn, secondConn = secondConn, conn
		}
		invoke("SetSSOConfig", append(field(1, requestContext), field(2, field(1, []byte(input)))...))
		model := strings.TrimSpace(input)
		check(model)
		started := time.Now()
		if *immediate {
			require.True(t, saml(model), "immediate SAML consistency after step %d", i)
		} else {
			require.Eventually(t, func() bool { check(model); return saml(model) }, 135*time.Second, time.Second, "SAML convergence step %d", i)
			t.Logf("step %d SAML convergence=%s", i, time.Since(started))
			for stable := 0; stable < 3; stable++ {
				check(model)
				require.True(t, saml(model))
				time.Sleep(time.Second)
			}
		}
	}

}

// This helper serves metadata in the fixture app's network namespace. It never
// starts a BuildBuddy component; the released app is owned by harness.Start.
func TestMetadataHelper(t *testing.T) {
	dir := os.Getenv("SSO_METADATA_HELPER")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "key.pem"))
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		entity := `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://metadata.fixture/` + strings.TrimPrefix(r.URL.Path, "/") + `"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://metadata.fixture/` + strings.TrimPrefix(r.URL.Path, "/") + `/login"/></IDPSSODescriptor></EntityDescriptor>`
		switch r.URL.Path {
		case "/first.xml":
			fmt.Fprint(w, entity)
		case "/second.xml":
			fmt.Fprint(w, `<EntitiesDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata">`+entity+`</EntitiesDescriptor>`)
		default:
			http.NotFound(w, r)
		}
	})}
	ready, err := json.Marshal("https://" + ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ready"), ready, 0644))
	require.NoError(t, server.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})))
}
