package execution_test

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

func TestUnknownOperationReturnsNotFound(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A syntactically valid operation name, never submitted to this fresh app.
	name := harness.UploadResourceName("unknown", harness.Digest([]byte("never executed")), false)
	s, err := f.Clients(harness.App).Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
	if err == nil {
		_, err = s.Recv()
	}
	t.Logf("WaitExecution(%q): %v", name, err)
	require.Equal(t, codes.NotFound, status.Code(err), "unknown operations must be rejected without waiting for the client's deadline")
}
