package authgroups_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestWaitExecutionRejectsOtherGroup(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a := f.Clients(harness.App)
	group := f.Web.CreateGroup(t, "other execution group")
	b := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, group, "other writer", []string{"CACHE_WRITE"}))
	root, err := a.UploadProto(ctx, "", &repb.Directory{})
	require.NoError(t, err)
	cmd, err := a.UploadProto(ctx, "", &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf 'private execution stdout'"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
	require.NoError(t, err)
	action, err := a.UploadProto(ctx, "", &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	s, err := a.Execution.Execute(ctx, &repb.ExecuteRequest{ActionDigest: action, SkipCacheLookup: true})
	require.NoError(t, err)
	name := ""
	for {
		op, err := s.Recv()
		require.NoError(t, err)
		name = op.Name
		if op.Done {
			require.Nil(t, op.GetError())
			result := &repb.ExecuteResponse{}
			require.NoError(t, op.GetResponse().UnmarshalTo(result))
			require.Zero(t, result.GetStatus().GetCode())
			require.Zero(t, result.GetResult().GetExitCode())
			break
		}
	}
	own, err := a.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
	require.NoError(t, err)
	op, err := own.Recv()
	require.NoError(t, err)
	require.True(t, op.Done)
	other, err := b.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
	if err == nil {
		leaked, recvErr := other.Recv()
		err = recvErr
		if leaked != nil {
			t.Logf("other group received operation: %v", leaked)
		}
	}
	require.Contains(t, []codes.Code{codes.PermissionDenied, codes.NotFound}, status.Code(err), "another group must not receive execution metadata/results")
}
