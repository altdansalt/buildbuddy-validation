package userkeys

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func certificateConfig(t *testing.T) (harness.Config, []string, *x509.Certificate) {
	t.Helper()
	private, e := rsa.GenerateKey(rand.Reader, 2048)
	must(t, e)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, DNSNames: []string{"app", "localhost"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	must(t, e)
	ca, e := x509.ParseCertificate(der)
	must(t, e)
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	key := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(private)}))
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "cert.pem"), []byte(cert), 0644))
	must(t, os.WriteFile(filepath.Join(dir, "key.pem"), []byte(key), 0644))
	return harness.Config{"enable_ssl": true, "cert_file": "/fixture-tls/cert.pem", "key_file": "/fixture-tls/key.pem", "client_ca_cert": cert, "client_ca_key": key}, []string{"-v", dir + ":/fixture-tls:ro"}, ca
}
func checkCertificate(t *testing.T, ca *x509.Certificate, id, cert, key string) {
	t.Helper()
	pair, e := tls.X509KeyPair([]byte(cert), []byte(key))
	must(t, e)
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	must(t, e)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, e = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	must(t, e)
	if leaf.Subject.SerialNumber != id {
		t.Fatalf("certificate identity %q want %q", leaf.Subject.SerialNumber, id)
	}
}
