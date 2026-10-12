package authgroups_test

import (
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

func TestAuthorizedExecutorBelongsToKeyGroup(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"remote_execution": harness.Config{"require_executor_authorization": true}}}})
	other := f.Web.CreateGroup(t, "group without executor")
	harness.Start(t, harness.Options{Components: []harness.Component{harness.Executor}, AppTarget: f.Endpoint(harness.App).InternalGRPC, DockerArgs: map[harness.Component][]string{harness.Executor: {"--network", f.Network}}, Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"api_key": f.APIKey}}}})
	var nodes []any
	require.Eventually(t, func() bool {
		r, err := f.Web.RPC("GetExecutionNodes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		if err != nil {
			t.Log(err)
			return false
		}
		nodes, _ = r["executor"].([]any)
		return len(nodes) > 0
	}, 30*time.Second, 500*time.Millisecond)
	require.Len(t, nodes, 1)
	r, err := f.Web.RPC("GetExecutionNodes", map[string]any{"requestContext": f.Web.RequestContext(other)})
	require.NoError(t, err)
	empty, _ := r["executor"].([]any)
	require.Empty(t, empty, "executor registered with group A key must not be assigned to group B")
}
