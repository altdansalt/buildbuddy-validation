package smoke_test

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

func TestExecuteAndFetchOutput(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor},
		// Exercise overridable ports and server type as well as the default proxy fixture.
		Config: map[harness.Component]harness.Config{
			harness.App:      {"port": 18080, "grpc_port": 11985, "server_type": "smoke-app"},
			harness.Executor: {"port": 18081},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.True(t, caps.GetExecutionCapabilities().GetExecEnabled())
	instance := "execution-smoke"
	input, err := c.Upload(ctx, instance, []byte("hello from a released executor\n"))
	require.NoError(t, err)
	root, err := c.UploadProto(ctx, instance, &repb.Directory{Files: []*repb.FileNode{{Name: "input.txt", Digest: input}}})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{
		Arguments:   []string{"/bin/sh", "-c", "cat input.txt > output.txt; printf 'done\\n'"},
		OutputPaths: []string{"output.txt"},
		Platform:    &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}},
	})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	var result *repb.ExecuteResponse
	for {
		op, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError(), "operation %s failed", op.Name)
		result = &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(result))
		break
	}
	require.NotNil(t, result, "stream ended without a completed operation")
	require.Zero(t, result.GetStatus().GetCode(), "%v", result.GetStatus())
	require.Zero(t, result.GetResult().GetExitCode())
	require.Len(t, result.GetResult().GetOutputFiles(), 1)
	out := result.Result.OutputFiles[0]
	require.Equal(t, "output.txt", out.Path)
	b, err := c.Read(ctx, instance, out.Digest)
	require.NoError(t, err)
	require.Equal(t, []byte("hello from a released executor\n"), b)
	if result.Result.StdoutDigest != nil {
		b, err = c.Read(ctx, instance, result.Result.StdoutDigest)
		require.NoError(t, err)
	} else {
		b = result.Result.StdoutRaw
	}
	require.Equal(t, []byte("done\n"), b)
}
