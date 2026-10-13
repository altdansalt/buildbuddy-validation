package timeouts_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// A TERM handler can finish the parent before its TERM-ignoring child. The
// timeout still needs to stop the child, even when the parent's exit is zero.
func TestGracefulTimeoutChild(t *testing.T) {
	f := startFixture(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	marker := fmt.Sprintf("/tmp/graceful-timeout-child-%s", f.ID)
	script := fmt.Sprintf("trap 'printf terminated; exit 0' TERM; (trap '' TERM; sleep 5; printf survived > %s) </dev/null >/dev/null 2>&1 & printf started; while :; do sleep 0.1; done", marker)
	r := execute(t, ctx, c, upload(t, ctx, c, script, time.Second, &repb.Platform_Property{Name: "termination-grace-period", Value: "2s"}))
	require.Equal(t, int32(codes.DeadlineExceeded), r.GetStatus().GetCode())
	require.Equal(t, "startedterminated", stdout(t, ctx, c, r))
	probe := execute(t, ctx, c, upload(t, ctx, c, fmt.Sprintf("sleep 6; if test -e %s; then cat %s; rm -f %s; exit 9; fi; printf clean", marker, marker, marker), 10*time.Second))
	require.Zero(t, probe.GetStatus().GetCode())
	require.Zero(t, probe.GetResult().GetExitCode(), "descendant survived past timeout and grace period: %s", stdout(t, ctx, c, probe))
}
