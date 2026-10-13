package user_lists_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Serve valid metadata inside the remote test. Trust only the generated local
// certificate in the app container; no external identity provider is contacted.
func metadataFixture(t *testing.T) (string, []string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "metadata.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	url := fmt.Sprintf("https://127.0.0.1:%d/metadata", listener.Addr().(*net.TCPAddr).Port)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		fmt.Fprintf(w, `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="%s"/></IDPSSODescriptor></EntityDescriptor>`, url, url)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "metadata-ca.pem")
	require.NoError(t, os.WriteFile(path, certPEM, 0644))
	return url, []string{"--volume=" + path + ":/metadata-ca.pem:ro", "--env=SSL_CERT_FILE=/metadata-ca.pem"}
}

type scimGroup struct {
	ID      string `json:"id"`
	Name    string `json:"displayName"`
	Members []struct {
		ID    string `json:"value"`
		Email string `json:"display"`
	} `json:"members"`
}

func (g scimGroup) list() list {
	l := list{ID: g.ID, Name: g.Name}
	for _, m := range g.Members {
		l.Members = append(l.Members, member{ID: m.ID, Email: m.Email})
	}
	normalize(&l)
	return l
}

// The metadata fetcher deliberately rejects private addresses. A test-local
// relay in the app network namespace permits the documented localhost test
// option without contacting a public identity provider.
func startMetadataRelay(t *testing.T, f *harness.Fixture, metadataURL string) {
	t.Helper()
	args := []string{"inspect", "--format", `{{.State.Pid}} {{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}`, f.Endpoint(harness.App).Container}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		sockets, globErr := filepath.Glob("/tmp/bbval-docker-*/docker.sock")
		require.NoError(t, globErr)
		for _, socket := range sockets {
			out, err = exec.Command("docker", append([]string{"--host", "unix://" + socket}, args...)...).CombinedOutput()
			if err == nil {
				break
			}
		}
	}
	require.NoError(t, err, string(out))
	parts := strings.Fields(string(out))
	require.Len(t, parts, 2)
	port := strings.Split(strings.TrimPrefix(metadataURL, "https://"), ":")[1]
	port = strings.Split(port, "/")[0]
	binary, err := os.Executable()
	require.NoError(t, err)
	relay := exec.Command("sudo", "-n", "nsenter", "--target", parts[0], "--net", "env", "BBVAL_METADATA_RELAY="+port, "BBVAL_METADATA_TARGET="+parts[1]+":"+port, binary, "-test.run=^TestMetadataRelay$")
	relay.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	log, err := os.Create(filepath.Join(f.OutputDir, "metadata-relay.log"))
	require.NoError(t, err)
	relay.Stdout = log
	relay.Stderr = log
	require.NoError(t, relay.Start())
	t.Cleanup(func() { syscall.Kill(-relay.Process.Pid, syscall.SIGKILL); relay.Wait(); log.Close() })
	time.Sleep(200 * time.Millisecond)
}
func TestMetadataRelay(t *testing.T) {
	port := os.Getenv("BBVAL_METADATA_RELAY")
	if port == "" {
		return
	}
	_, err := strconv.Atoi(port)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	require.NoError(t, err)
	for {
		client, err := listener.Accept()
		require.NoError(t, err)
		go func() {
			defer client.Close()
			upstream, err := net.DialTimeout("tcp", os.Getenv("BBVAL_METADATA_TARGET"), 5*time.Second)
			if err != nil {
				return
			}
			defer upstream.Close()
			go io.Copy(upstream, client)
			io.Copy(client, upstream)
		}()
	}
}

// Exercises SCIM v2 Groups as both a reader and a writer of the same state the
// gRPC and UI APIs expose. SAML + SCIM are non-default; the metadata document
// is served by the test itself.
func TestSCIMGroups(t *testing.T) {
	metadataURL, dockerArgs := metadataFixture(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App},
		DockerArgs: map[harness.Component][]string{harness.App: dockerArgs},
		Args:       map[harness.Component][]string{harness.App: {"--http.client.allow_localhost=true"}},
		Config:     map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_user_lists": true, "enable_scim": true}}}})
	startMetadataRelay(t, f, metadataURL)
	r := newRig(t, f)
	_, err := r.web("SetSSOConfig", map[string]any{"config": map[string]any{"samlIdpMetadataUrl": metadataURL}})
	require.NoError(t, err)
	r.scimKey = r.keys[0]
	r.scimBase = "http://" + f.Endpoint(harness.App).HTTP
	r.attached = false
	model := map[string]list{}
	deleted := []string{}
	r.check(model, deleted)
	member1 := map[string]any{"value": r.identity.ID, "display": r.identity.Email}
	scimOp := func(id string, ops ...map[string]any) {
		var g scimGroup
		require.Equal(t, 200, r.scim("PATCH", "/"+id, map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"}, "Operations": ops}, &g))
		_ = g
	}
	var created scimGroup
	require.Equal(t, 200, r.scim("POST", "", map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:Group"}, "displayName": "scim one", "members": []any{member1}}, &created))
	id := created.ID
	model[id] = list{ID: id, Name: "scim one", Members: []member{r.identity}}
	r.check(model, deleted)
	// SCIM write, gRPC write, SCIM write, UI-visible every step.
	scimOp(id, map[string]any{"op": "replace", "path": "displayName", "value": "scim renamed"})
	model[id] = list{ID: id, Name: "scim renamed", Members: model[id].Members}
	r.check(model, deleted)
	r.must(1, "UpdateUserList", field(2, join(str(1, id), str(2, "grpc renamed"))))
	model[id] = list{ID: id, Name: "grpc renamed", Members: model[id].Members}
	r.check(model, deleted)
	scimOp(id, map[string]any{"op": "remove", "path": "members", "value": []any{member1}})
	model[id] = list{ID: id, Name: "grpc renamed"}
	r.check(model, deleted)
	require.Equal(t, 200, r.scim("PUT", "/"+id, map[string]any{"displayName": "put name", "members": []any{member1}}, nil))
	model[id] = list{ID: id, Name: "put name", Members: []member{r.identity}}
	r.check(model, deleted)
	// A second list created over gRPC is visible to SCIM, and deleted over SCIM it vanishes from gRPC/UI.
	id2 := r.create(0, "grpc made", model)
	model[id2] = list{ID: id2, Name: "grpc made"}
	r.check(model, deleted)
	require.Equal(t, 204, r.scim("DELETE", "/"+id2, nil, nil))
	delete(model, id2)
	deleted = append(deleted, id2)
	r.check(model, deleted)
	r.must(0, "DeleteUserList", str(2, id))
	delete(model, id)
	deleted = append(deleted, id)
	r.check(model, deleted)
	require.Equal(t, 404, r.scim("DELETE", "/"+id, nil, nil))
	require.Equal(t, 404, r.scim("DELETE", "/"+id2, nil, nil))
	var again scimGroup
	require.Equal(t, 200, r.scim("POST", "", map[string]any{"displayName": "scim one"}, &again))
	require.NotEqual(t, id, again.ID)
	model[again.ID] = list{ID: again.ID, Name: "scim one"}
	r.check(model, deleted)
	time.Sleep(2 * time.Second)
	r.check(model, deleted)
}
