package isolation_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
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

func TestBareStateBetweenActions(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Logf("KVM unavailable on test runner (%v); exercising none isolation", err)
	} else {
		t.Log("KVM device exists; Firecracker additionally requires executor device passthrough and provider configuration")
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	// Non-recycled actions have separate workspaces and managed temporary dirs.
	first := execute(t, c, `set -eu; test -n "$TMPDIR"; printf '%s\n%s\n' "$PWD" "$TMPDIR"; mkdir -p stale/sub; echo stale > stale/sub/file; ln -s input stale-link; echo changed > input; echo secret > "$TMPDIR/marker"`, false, []*repb.Command_EnvironmentVariable{{Name: "ACTION_SECRET", Value: "first-only"}})
	paths := strings.Split(strings.TrimSpace(first), "\n")
	require.Len(t, paths, 2)
	// Cleanup is asynchronous after completion; poll only fixture-local paths.
	script := fmt.Sprintf(`set -eu; test -z "${ACTION_SECRET-}"; test "$(cat input)" = original; test ! -e stale; test ! -L stale-link; test "$PWD" != '%s'; test "$TMPDIR" != '%s'; i=0; while test -e '%s' || test -e '%s'; do i=$((i+1)); test "$i" -lt 100 || { echo cleanup-did-not-complete >&2; exit 1; }; sleep 0.1; done; echo clean`, paths[0], paths[1], paths[0], paths[1])
	require.Equal(t, "clean\n", execute(t, c, script, false, nil))
}

func TestRecycledWorkspaceIsClean(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"runner_pool": harness.Config{"max_runner_count": 1}}}}})
	c := f.Clients(harness.App)
	seen := make(map[string]bool)
	reused := false
	for i := 0; i < 8; i++ {
		// Each action dirties both declared input and undeclared nested/symlink state.
		// Default preserve-workspace=false must remove these before pool reuse.
		out := execute(t, c, fmt.Sprintf(`set -eu; test "$(cat input)" = original; test ! -e stale; test ! -L stale-link; test -z "${PREVIOUS_ACTION-}"; printf '%%s\n' "$PWD"; mkdir -p stale/nested; echo %d > stale/nested/file; ln -s input stale-link; echo mutated > input; export PREVIOUS_ACTION=yes`, i), true, nil)
		path := strings.TrimSpace(out)
		if seen[path] {
			reused = true
		}
		seen[path] = true
		// Allow the pool's asynchronous post-completion recycling to finish.
		time.Sleep(250 * time.Millisecond)
	}
	require.True(t, reused, "no workspace reuse observed; cleanup assertions did not exercise recycling")
}
