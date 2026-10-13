package envargs_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

const instance = "env-args"

func fixture(t *testing.T) *harness.Clients {
	t.Helper()
	privateDaemon(t)
	var f *harness.Fixture
	// Register before Start so this runs after the harness saves container logs.
	t.Cleanup(func() {
		if f == nil {
			return
		}
		logs, err := filepath.Glob(filepath.Join(f.OutputDir, "*.logs.txt"))
		require.NoError(t, err)
		for _, path := range logs {
			b, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotContains(t, string(b), "panic:", "container log %s", path)
			require.NotContains(t, string(b), "fatal error:", "container log %s", path)
			for _, line := range strings.Split(string(b), "\n") {
				if strings.Contains(line, "executable file not found") {
					t.Logf("container log: %s", line)
				}
			}
		}
	})
	f = harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	return f.Clients(harness.App)
}
func upload(t *testing.T, c *harness.Clients, d *repb.Directory) *repb.Digest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	digest, err := c.UploadProto(ctx, instance, d)
	require.NoError(t, err)
	return digest
}
func execute(t *testing.T, c *harness.Clients, root *repb.Digest, cmd *repb.Command) *repb.ExecuteResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if cmd.Platform == nil {
		cmd.Platform = &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}
	}
	cd, err := c.UploadProto(ctx, instance, cmd)
	require.NoError(t, err)
	ad, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cd, InputRootDigest: root, Timeout: durationpb.New(20 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	s, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: ad, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	if err != nil {
		return &repb.ExecuteResponse{Status: status.Convert(err).Proto()}
	}
	for {
		op, err := s.Recv()
		require.NotEqual(t, io.EOF, err, "execution ended without result")
		if err != nil {
			return &repb.ExecuteResponse{Status: status.Convert(err).Proto()}
		}
		if !op.Done {
			continue
		}
		if op.GetError() != nil {
			return &repb.ExecuteResponse{Status: op.GetError()}
		}
		r := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(r))
		t.Logf("execution status=%v exit=%d", r.GetStatus(), r.GetResult().GetExitCode())
		return r
	}
}
func stdout(t *testing.T, c *harness.Clients, r *repb.ExecuteResponse) string {
	t.Helper()
	require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
	require.NotNil(t, r.Result)
	require.Zero(t, r.Result.ExitCode)
	if r.Result.StdoutDigest == nil {
		return string(r.Result.StdoutRaw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := c.Read(ctx, instance, r.Result.StdoutDigest)
	require.NoError(t, err)
	return string(b)
}
func scriptTree(t *testing.T, c *harness.Clients) *repb.Digest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, err := c.Upload(ctx, instance, []byte("#!/bin/sh\nprintf 'input-tool:%s\\n' \"$1\"\n"))
	require.NoError(t, err)
	child := upload(t, c, &repb.Directory{Files: []*repb.FileNode{{Name: "envargs-tool", Digest: d, IsExecutable: true}}})
	return upload(t, c, &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "tools", Digest: child}}})
}
