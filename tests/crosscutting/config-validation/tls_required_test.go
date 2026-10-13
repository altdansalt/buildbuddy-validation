package configvalidation

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTLSWithoutCertificateSourceRejectsStartup(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, DockerArgs: map[harness.Component][]string{harness.App: {"--publish", "127.0.0.1::8081"}}})
	d := fixtureDocker(t, f)
	original := baseline(t, f, harness.App)
	config := replaceSetting(t, original, "enable_ssl: false", "enable_ssl: true")
	// Disable automatic certificate sources explicitly; no ACME/cloud call.
	config = strings.Replace(config, "ssl:\n", "ssl:\n    self_signed: false\n    use_acme: false\n", 1)
	since := restartConfig(t, f, d, harness.App, config)
	name := f.Endpoint(harness.App).Container
	// This also diagnoses the accepted-but-unusable secure listener. A fixed
	// release should exit with a configuration error instead of becoming ready.
	if d.run(t, "inspect", "--format", "{{.State.Running}}", name) == "true" {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := (&http.Client{Timeout: time.Second}).Get(f.Endpoint(harness.App).HTTP + "/readyz?server-type=app")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		address := publishedAddress(t, f, d, "8081")
		err := tlsHandshake(address)
		t.Logf("TLS handshake with no certificate source: %v", err)
		if err == nil {
			t.Error("unexpectedly served TLS without any certificate source")
		}
	}
	assertStartupRejected(t, f, d, harness.App, since, "SSL")
}
