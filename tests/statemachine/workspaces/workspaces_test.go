package workspaces

import (
	"strings"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

// This is a prerequisite check, not overlay conformance coverage. A repository
// tree must be readable before deletion and untouched-content oracles are valid.
func TestRepositoryReadPrerequisite(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App},
		Config: map[harness.Component]harness.Config{
			harness.App: {"workspace": harness.Config{"enabled": true}},
		},
	})
	// The URL identifies a prospective fixture repository. No request is sent to
	// GitHub: the released app rejects the read before constructing a client.
	repo := map[string]any{"repoUrl": "https://github.com/fixture/workspace", "branch": "main"}
	for _, tc := range []struct {
		method string
		node   string
	}{
		{"GetWorkspaceDirectory", "directory"},
		{"GetWorkspaceFile", "file"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			node := map[string]any{}
			if tc.node == "file" {
				node["path"] = "README.md"
			}
			_, err := f.Web.RPC(tc.method, map[string]any{
				"requestContext": f.Web.RequestContext(f.GroupID),
				"workspace":      "fixture-overlay", "repo": repo, tc.node: node,
			})
			if err == nil || !strings.Contains(err.Error(), "No GitHub app configured") {
				t.Fatalf("expected documented GitHub prerequisite; got %v", err)
			}
			t.Logf("Conformance blocked by fixture prerequisite: %v", err)
		})
	}
}
