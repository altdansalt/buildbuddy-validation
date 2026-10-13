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
	"fmt"
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
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--http.client.allow_localhost=true"}}, DockerArgs: map[harness.Component][]string{harness.App: {"--volume", certPath + ":/fixture-ca.pem:ro", "--env", "SSL_CERT_FILE=/fixture-ca.pem"}}})
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
	groupSetup, err := f.Web.RPC("CreateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "name": "SSO conformance", "urlIdentifier": "sso-conformance"})
	require.NoError(t, err)
	groupID, ok := groupSetup["id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, groupID)
	f.GroupID = groupID
	adminKey := f.Web.CreateAPIKey(t, f.GroupID, "sso-admin", []string{"ORG_ADMIN"})
	conn := harness.Dial(t, f.Endpoint(harness.App).GRPC, adminKey).Conn
	requestContext := field(2, []byte(f.GroupID))
	invoke := func(method string, req []byte) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var response []byte
		require.NoError(t, conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, &req, &response, grpc.ForceCodec(wireCodec{})))
		return response
	}
	check := func(model string) {
		got := invoke("GetSSOConfig", field(1, requestContext))
		require.Equal(t, model, string(readField(t, readField(t, got, 2), 1)), "gRPC state")
		web, err := f.Web.RPC("GetSSOConfig", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		require.NoError(t, err)
		config, ok := web["config"].(map[string]any)
		require.True(t, ok, "%v", web)
		value, _ := config["samlIdpMetadataUrl"].(string)
		require.Equal(t, model, value, "UI protolet state")
		group := invoke("GetGroup", append(field(2, requestContext), field(1, []byte("sso-conformance"))...))
		require.Equal(t, model != "", readBool(t, group, 5), "gRPC public SSO enabled state")
		groupWeb, err := f.Web.RPC("GetGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "urlIdentifier": "sso-conformance"})
		require.NoError(t, err)
		enabled, _ := groupWeb["ssoEnabled"].(bool)
		require.Equal(t, model != "", enabled, "UI public SSO enabled state")
	}
	check("")
	for i, input := range []string{" \t" + base + "/first.xml\n", base + "/first.xml", base + "/second.xml", base + "/second.xml", "", "", " \t\n"} {
		t.Logf("transition_%d", i)
		{
			invoke("SetSSOConfig", append(field(1, requestContext), field(2, field(1, []byte(input)))...))
			check(strings.TrimSpace(input))
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
		entity := `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://metadata.fixture/` + strings.TrimPrefix(r.URL.Path, "/") + `"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://metadata.fixture/login"/></IDPSSODescriptor></EntityDescriptor>`
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
