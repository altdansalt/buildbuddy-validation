package capabilities_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func bounded(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Execute must be implemented on any endpoint that advertises exec_enabled.
// A missing action may fail, but an unknown Execution service is inconsistent.
func checkExecutionService(t *testing.T, c *harness.Clients, requireEnabled bool) {
	t.Helper()
	ctx := bounded(t)
	caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	if requireEnabled {
		require.True(t, caps.GetExecutionCapabilities().GetExecEnabled())
	}
	if !caps.GetExecutionCapabilities().GetExecEnabled() {
		return // Correctly withholding the advertisement fixes the proxy defect.
	}
	s, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{ActionDigest: harness.Digest([]byte("missing-action")), DigestFunction: repb.DigestFunction_SHA256})
	if err == nil {
		_, err = s.Recv()
	}
	require.NotEqual(t, codes.Unimplemented, status.Code(err), "exec_enabled=true but Execute is unavailable: %v", err)
}
