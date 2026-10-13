package stdioexit_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

func execute(t *testing.T, c *harness.Clients, ctx context.Context, instance, script string, stdout, stderr []byte) (*repb.Digest, *repb.ExecuteResponse) {
	t.Helper()
	var files []*repb.FileNode
	for i, data := range [][]byte{stdout, stderr} {
		d, err := c.Write(ctx, instance, data, false)
		require.NoError(t, err)
		files = append(files, &repb.FileNode{Name: fmt.Sprintf("stream%d", i), Digest: d})
	}
	root, err := c.UploadProto(ctx, instance, &repb.Directory{Files: files})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second)})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	for {
		op, err := stream.Recv()
		require.NotEqual(t, io.EOF, err, "stream ended without completion")
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		require.NotNil(t, op.GetResponse())
		response := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(response))
		return action, response
	}
}

func checkStreams(t *testing.T, c *harness.Clients, ctx context.Context, instance string, r *repb.ActionResult, stdout, stderr []byte) {
	t.Helper()
	require.NotNil(t, r)
	for i, stream := range []struct {
		raw    []byte
		digest *repb.Digest
		want   []byte
	}{{r.StdoutRaw, r.StdoutDigest, stdout}, {r.StderrRaw, r.StderrDigest, stderr}} {
		if stream.digest != nil {
			require.Equal(t, harness.Digest(stream.want), stream.digest, "stream %d digest", i)
			got, err := c.Read(ctx, instance, stream.digest)
			require.NoError(t, err)
			require.True(t, bytes.Equal(stream.want, got), "stream %d CAS bytes differ (want %d, got %d)", i, len(stream.want), len(got))
		} else {
			require.True(t, bytes.Equal(stream.want, stream.raw), "stream %d omitted or corrupted", i)
		}
		if len(stream.raw) > 0 {
			require.True(t, bytes.Equal(stream.want, stream.raw), "stream %d inline bytes differ", i)
		}
	}
	require.Empty(t, r.OutputFiles)
	require.Empty(t, r.OutputDirectories)
	require.Empty(t, r.OutputSymlinks)
}

func TestStdioAndExitCodes(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	binary := make([]byte, 256)
	for i := range binary {
		binary[i] = byte(i)
	}
	cases := []struct {
		name           string
		stdout, stderr []byte
		code           int32
	}{
		{"empty", nil, nil, 0}, {"binary", binary, append([]byte("stderr\x00"), binary...), 0},
		{"large", bytes.Repeat(binary, 32769), bytes.Repeat(binary, 16385), 0},
		{"stdout_only", binary, nil, 0}, {"stderr_only", nil, binary, 0},
		{"exit1", binary, []byte("failure\n"), 1}, {"exit42", []byte("out\n"), binary, 42}, {"exit255", binary, binary, 255},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			instance := "stdio-exit/" + tc.name
			action, response := execute(t, c, ctx, instance, fmt.Sprintf("cat stream0 & cat stream1 >&2 & wait; exit %d", tc.code), tc.stdout, tc.stderr)
			require.Zero(t, response.GetStatus().GetCode(), "%v", response.GetStatus())
			require.False(t, response.CachedResult)
			require.NotNil(t, response.Result)
			require.Equal(t, tc.code, response.Result.ExitCode)
			checkStreams(t, c, ctx, instance, response.Result, tc.stdout, tc.stderr)
			require.NotNil(t, response.Result.ExecutionMetadata)
			if tc.code != 0 {
				return
			} // Failed commands need not be cached.
			for _, inline := range []bool{false, true} {
				cached, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action, InlineStdout: inline, InlineStderr: inline, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				require.Equal(t, tc.code, cached.ExitCode)
				checkStreams(t, c, ctx, instance, cached, tc.stdout, tc.stderr)
				t.Logf("inline=%t stdout raw=%d digest=%v stderr raw=%d digest=%v", inline, len(cached.StdoutRaw), cached.StdoutDigest, len(cached.StderrRaw), cached.StderrDigest)
			}
		})
	}
}

// REAPI does not prescribe a POSIX signal-to-exit-code mapping. The pinned bare
// runner uses a negative exit code and a non-OK execution status; preserve the
// diagnostic streams even when the command does not complete normally.
func TestSignalTerminationPreservesStdio(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	for _, signal := range []string{"TERM", "KILL", "ABRT"} {
		t.Run(signal, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			out, errout := []byte("before signal\x00\xff"), []byte("diagnostic\x00\xfe")
			_, response := execute(t, c, ctx, "stdio-signals/"+signal, "ulimit -c 0; cat stream0; cat stream1 >&2; kill -"+signal+" $$", out, errout)
			require.NotZero(t, response.GetStatus().GetCode(), "signal termination must not report successful execution")
			require.NotNil(t, response.Result)
			require.Less(t, response.Result.ExitCode, int32(0))
			checkStreams(t, c, ctx, "stdio-signals/"+signal, response.Result, out, errout)
			t.Logf("signal=%s status=%v exit_code=%d", signal, response.Status, response.Result.ExitCode)
		})
	}
}
