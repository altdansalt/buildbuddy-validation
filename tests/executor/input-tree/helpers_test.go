package inputtree_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

func execute(t *testing.T, c *harness.Clients, root *repb.Directory, script string) *repb.ExecuteResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rd, err := c.UploadProto(ctx, "input-tree", root)
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, "input-tree", &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
	require.NoError(t, err)
	ad, err := c.UploadProto(ctx, "input-tree", &repb.Action{CommandDigest: cmd, InputRootDigest: rd, Timeout: durationpb.New(30 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "input-tree", ActionDigest: ad, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	for {
		op, err := stream.Recv()
		if err == io.EOF {
			t.Fatal("no completed operation")
		}
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		r := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(r))
		return r
	}
}
func success(t *testing.T, c *harness.Clients, root *repb.Directory, script string) {
	t.Helper()
	r := execute(t, c, root, "set -eu\n"+script)
	require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
	require.NotNil(t, r.Result)
	stderr := r.Result.StderrRaw
	if r.Result.StderrDigest != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var err error
		stderr, err = c.Read(ctx, "input-tree", r.Result.StderrDigest)
		require.NoError(t, err)
	}
	require.Zero(t, r.Result.ExitCode, "stderr: %s", stderr)
}
