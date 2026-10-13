package configvalidation

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"strings"
	"testing"
)

func TestRejectedReloadPreservesSecurityConfig(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{
		harness.App: {"app": harness.Config{"strict_csp_enabled": true}},
	}})
	d := fixtureDocker(t, f)
	original := baseline(t, f, harness.App)
	waitCSP(t, f, true)
	// A typed YAML error is rejected before any new settings can be committed.
	// It must not reset the previous, valid security policy to flag defaults.
	invalid := portLine.ReplaceAllString(original, "port: not-an-integer")
	if invalid == original {
		t.Fatal("port mutation did not match baseline")
	}
	reloadConfig(t, f, d, harness.App, invalid, true)
	got := hasCSP(t, f)
	// Prove that the same fixture and handler recover using a valid reload.
	reloadConfig(t, f, d, harness.App, original, false)
	waitCSP(t, f, true)
	if !got {
		t.Fatal("rejected reload removed the active Content-Security-Policy header")
	}
	if !strings.Contains(original, "strict_csp_enabled: true") {
		t.Fatal("baseline did not enable CSP")
	}
}
