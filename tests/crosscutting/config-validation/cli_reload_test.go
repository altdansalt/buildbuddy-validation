package configvalidation

import (
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

func TestSIGHUPPreservesCLIOverride(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App},
		Config: map[harness.Component]harness.Config{
			harness.App: {"app": harness.Config{"strict_csp_enabled": false}},
		},
		Args: map[harness.Component][]string{
			harness.App: {"--app.strict_csp_enabled=true"},
		},
		DockerArgs: map[harness.Component][]string{
			harness.App: {"--publish", "127.0.0.1::9090"},
		},
	})
	d := fixtureDocker(t, f)
	// The contradictory YAML value proves startup honors the CLI override.
	waitCSP(t, f, true)
	config := replaceSetting(t, baseline(t, f, harness.App), "server_type: app", "server_type: cli-reload-complete")
	reloadConfig(t, f, d, harness.App, config, false)
	// Observe a YAML-only flag change before checking the overridden flag.
	waitReloadMarker(t, publishedAddress(t, f, d, "9090"), "cli-reload-complete")
	if !hasCSP(t, f) {
		t.Fatal("CLI precedence after reload lost the expected CSP header")
	}
}
