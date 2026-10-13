package randexec_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

func minimalOutputCase(t *testing.T, script string, outputs []string) {
	t.Helper()
	f := bareFixture(t)
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tc := testCase{Input: "payload", Command: &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, OutputPaths: outputs, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "Arch", Value: "amd64"}, {Name: "OSFamily", Value: "linux"}, {Name: "workload-isolation-type", Value: "none"}}}}}
	expected := reference(t, ctx, tc)
	action, err := c.UploadProto(ctx, "round2", uploadCase(t, ctx, c, tc))
	require.NoError(t, err)
	response := execute(t, ctx, c, "round2", action, true)
	t.Logf("ActionResult: %s", response.Result)
	require.Equal(t, expected, normalized(t, ctx, c, "round2", response.Result))
}
