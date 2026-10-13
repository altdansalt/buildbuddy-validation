package timeouts_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestDockerGracefulTimeout(t *testing.T) {
	f := dockerFixture(t)
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	control := execute(t, ctx, c, upload(t, ctx, c, "trap 'printf terminated; exit 0' TERM; printf started; kill -TERM $$", 10*time.Second, dockerProps()...))
	require.Zero(t, control.GetStatus().GetCode())
	require.Zero(t, control.GetResult().GetExitCode())
	require.Equal(t, "startedterminated", stdout(t, ctx, c, control), "the image must support the TERM handler")
	props := append(dockerProps(), &repb.Platform_Property{Name: "termination-grace-period", Value: "2s"})
	r := execute(t, ctx, c, upload(t, ctx, c, "trap 'printf terminated; exit 0' TERM; printf started; while :; do sleep 0.1; done", time.Second, props...))
	require.Equal(t, int32(codes.DeadlineExceeded), r.GetStatus().GetCode())
	require.Equal(t, "startedterminated", stdout(t, ctx, c, r), "termination grace period should deliver TERM before force kill")
}
