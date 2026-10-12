package actioncache_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func setup(t *testing.T) (context.Context, *harness.Clients) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx, f.Clients(harness.App)
}

// Upload the real Command and Action required by UpdateActionResult's contract.
func action(t *testing.T, ctx context.Context, c *harness.Clients, instance, label string) *repb.Digest {
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"echo", label}, OutputPaths: []string{"dir", "empty", "out"}})
	require.NoError(t, err)
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	d, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root})
	require.NoError(t, err)
	return d
}
func put(t *testing.T, ctx context.Context, c *harness.Clients, instance string, d *repb.Digest, r *repb.ActionResult) {
	_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, ActionResult: r})
	require.NoError(t, err)
}
