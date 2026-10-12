package execution_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMissingCommandIdentifiesCommandDigest(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	command := harness.Digest([]byte("command deliberately not uploaded"))
	root, err := c.UploadProto(ctx, "missing-command", &repb.Directory{})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, "missing-command", &repb.Action{CommandDigest: command, InputRootDigest: root})
	require.NoError(t, err)
	// Establish that the action is available and only the command is absent.
	missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "missing-command", BlobDigests: []*repb.Digest{action, root, command}})
	require.NoError(t, err)
	require.Equal(t, []*repb.Digest{command}, missing.MissingBlobDigests)
	s, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "missing-command", ActionDigest: action, SkipCacheLookup: true})
	if err == nil {
		_, err = s.Recv()
	}
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
	t.Logf("action=%s/%d command=%s/%d error=%v", action.Hash, action.SizeBytes, command.Hash, command.SizeBytes, err)
	var subjects []string
	for _, detail := range status.Convert(err).Details() {
		if pf, ok := detail.(*errdetails.PreconditionFailure); ok {
			for _, v := range pf.Violations {
				if v.Type == "MISSING" {
					subjects = append(subjects, v.Subject)
				}
			}
		}
	}
	require.Equal(t, []string{fmt.Sprintf("blobs/%s/%d", command.Hash, command.SizeBytes)}, subjects, "missing-blob repair must identify the absent command, not the uploaded action")
}
