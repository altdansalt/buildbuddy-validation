package actioncache_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestMissingStreamBlobs(t *testing.T) {
	ctx, c := setup(t)
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			d := action(t, ctx, c, "", stream)
			missing := harness.Digest([]byte("never uploaded " + stream))
			r := &repb.ActionResult{}
			if stream == "stdout" {
				r.StdoutDigest = missing
			} else {
				r.StderrDigest = missing
			}
			put(t, ctx, c, "", d, r)
			_, err := c.Read(ctx, "", missing)
			require.Equal(t, codes.NotFound, status.Code(err), "CAS control")
			got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
			require.Equal(t, codes.NotFound, status.Code(err), "dangling %s reference returned: %v", stream, got)
		})
	}
}
