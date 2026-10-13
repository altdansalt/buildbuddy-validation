package actioncache_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestNegativeOutputDigest(t *testing.T) {
	ctx, c := setup(t)
	d := action(t, ctx, c, "", "negative-output")
	invalid := harness.Digest([]byte("output"))
	invalid.SizeBytes = -1
	_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out", Digest: invalid}}}})
	if status.Code(err) == codes.InvalidArgument {
		return
	}
	require.NoError(t, err)
	got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
	t.Logf("GetActionResult: result=%v error=%v", got, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err), "UpdateActionResult accepted a negative output size")
}
