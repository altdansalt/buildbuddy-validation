package isolation_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
	"io"
	"testing"
	"time"
)

// All shell commands execute inside the fixture's released executor container.
func execute(t *testing.T, c *harness.Clients, script string, recycle bool, env []*repb.Command_EnvironmentVariable) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const instance = "isolation-state"
	input, err := c.Upload(ctx, instance, []byte("original\n"))
	require.NoError(t, err)
	root, err := c.UploadProto(ctx, instance, &repb.Directory{Files: []*repb.FileNode{{Name: "input", Digest: input}}})
	require.NoError(t, err)
	props := []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}
	if recycle {
		props = append(props, &repb.Platform_Property{Name: "recycle-runner", Value: "true"})
	}
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, EnvironmentVariables: env, Platform: &repb.Platform{Properties: props}})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	for {
		op, err := stream.Recv()
		if err == io.EOF {
			t.Fatal("execution ended without result")
		}
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		response := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(response))
		require.Zero(t, response.GetStatus().GetCode(), "%v", response.GetStatus())
		result := response.GetResult()
		require.NotNil(t, result)
		stderr := result.StderrRaw
		if result.StderrDigest != nil {
			stderr, err = c.Read(ctx, instance, result.StderrDigest)
			require.NoError(t, err)
		}
		require.Zero(t, result.ExitCode, "stderr: %s", stderr)
		stdout := result.StdoutRaw
		if result.StdoutDigest != nil {
			stdout, err = c.Read(ctx, instance, result.StdoutDigest)
			require.NoError(t, err)
		}
		return string(stdout)
	}
}
