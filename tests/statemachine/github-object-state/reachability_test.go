package github_object_state_test

import (
	"strings"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

// This is an integration prerequisite probe, not object-state conformance.
// The default fixture has no linked GitHub identity. Do not enable the GitHub
// app here: token validation can contact public GitHub even with enterprise_host.
func TestGitHubIntegrationPrerequisite(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	_, err := f.Web.RPC("GetGithubRepo", map[string]any{
		"requestContext": f.Web.RequestContext(f.GroupID),
		"owner":          "fixture-owner", "repo": "fixture-repo",
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "not implemented") {
		t.Fatalf("expected GitHub app integration prerequisite, got %v", err)
	}
	t.Logf("Object-state conformance unreachable with default fixture: %v", err)
}
