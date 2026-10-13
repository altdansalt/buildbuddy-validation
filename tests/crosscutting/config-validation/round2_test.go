package configvalidation

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

func TestConflictingAndFeatureRequiredSettings(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	d := fixtureDocker(t, f)
	app := baseline(t, f, harness.App)
	executor := baseline(t, f, harness.Executor)
	tls := replaceSetting(t, app, "enable_ssl: false", "enable_ssl: true")
	cases := []struct {
		name               string
		component          harness.Component
		config, diagnostic string
	}{
		{"default_isolation_not_enabled", harness.Executor, replaceSetting(t, executor, "default_isolation_type: none", "default_isolation_type: docker"), "not enabled for this executor"},
		{"unknown_default_isolation", harness.Executor, replaceSetting(t, executor, "default_isolation_type: none", "default_isolation_type: invalid-round2"), "default_isolation_type"},
		{"conflicting_ca_certificate_sources", harness.App, strings.Replace(tls, "ssl:\n", "ssl:\n    self_signed: true\n    client_ca_cert_file: /data/unused.pem\n    client_ca_cert: unused-inline\n    client_ca_key: unused-key\n", 1), "but not both"},
		{"tls_certificate_files_missing", harness.App, strings.Replace(tls, "ssl:\n", "ssl:\n    cert_file: /data/missing-cert.pem\n    key_file: /data/missing-key.pem\n", 1), "missing-cert.pem"},
		{"tls_reload_interval_nonpositive", harness.App, strings.Replace(tls, "ssl:\n", "ssl:\n    cert_file: /data/missing-cert.pem\n    key_file: /data/missing-key.pem\n    cert_reload_interval: 0s\n", 1), "ssl.cert_reload_interval must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			since := restartConfig(t, f, d, tc.component, tc.config)
			assertStartupRejected(t, f, d, tc.component, since, tc.diagnostic)
		})
	}
}

func TestSIGHUPValidReloadAndCLIPrecedence(t *testing.T) {
	// A dynamic HTTP security header proves that reload changes live behavior.
	// Startup-only fields such as listener ports are deliberately kept constant.
	for _, cli := range []bool{false, true} {
		t.Run(fmt.Sprintf("cli_%v", cli), func(t *testing.T) {
			opts := harness.Options{Components: []harness.Component{harness.App}, DockerArgs: map[harness.Component][]string{harness.App: {"--publish", "127.0.0.1::9090"}}}
			if cli {
				opts.Args = map[harness.Component][]string{harness.App: {"--app.strict_csp_enabled=true"}}
			}
			f := harness.Start(t, opts)
			d := fixtureDocker(t, f)
			original := baseline(t, f, harness.App)
			waitCSP(t, f, cli)
			r := rand.New(rand.NewSource(*seed))
			t.Logf("reload seed=%d", *seed)
			// Seeded ordering, with both states guaranteed and repeated transitions.
			values := []bool{false, true, false, true}
			r.Shuffle(len(values), func(i, j int) { values[i], values[j] = values[j], values[i] })
			for i, value := range values {
				t.Logf("reload=%d strict_csp_enabled=%v", i, value)
				config := strings.Replace(original, "app:\n", fmt.Sprintf("app:\n    strict_csp_enabled: %v\n", value), 1)
				marker := fmt.Sprintf("reload-%d-%d", *seed, i)
				config = replaceSetting(t, config, "server_type: app", "server_type: "+marker)
				reloadConfig(t, f, d, harness.App, config, false)
				// Read-only monitoring observation avoids accepting stale CSP
				// when consecutive samples (or CLI overrides) have equal values.
				waitReloadMarker(t, publishedAddress(t, f, d, "9090"), marker)
				waitCSP(t, f, value || cli)
			}
		})
	}
}

func TestTLSSelfSignedCertificateSource(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App},
		Config:     map[harness.Component]harness.Config{harness.App: {"ssl": harness.Config{"enable_ssl": true, "self_signed": true, "use_acme": false}}},
		DockerArgs: map[harness.Component][]string{harness.App: {"--publish", "127.0.0.1::8081"}},
	})
	d := fixtureDocker(t, f)
	if err := tlsHandshake(publishedAddress(t, f, d, "8081")); err != nil {
		t.Fatalf("self-signed TLS handshake: %v", err)
	}
}
