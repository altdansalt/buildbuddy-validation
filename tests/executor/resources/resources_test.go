package resources_test

import (
	"context"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

// execute always bypasses the action cache and bounds both scheduling and execution.
// REAPI ExecuteResponse.status represents service errors; a tool failure belongs
// in ActionResult.exit_code instead (remote_execution.proto, ExecuteResponse).
func execute(t *testing.T, c *harness.Clients, script string, estimates map[string]string) (*repb.ActionResult, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const instance = "executor-resources"
	props := map[string]string{"OSFamily": "linux", "Arch": "amd64", "workload-isolation-type": "none"}
	for k, v := range estimates {
		props[k] = v
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	platform := &repb.Platform{}
	for _, k := range keys {
		platform.Properties = append(platform.Properties, &repb.Platform_Property{Name: k, Value: props[k]})
	}
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", script + "\n# " + t.Name()}, Platform: platform})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(20 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	var response *repb.ExecuteResponse
	for {
		op, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError(), "operation error: %v", op.GetError())
		response = &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(response))
		break
	}
	require.NotNil(t, response, "stream ended without terminal result")
	require.Zero(t, response.GetStatus().GetCode(), "service error: %v", response.GetStatus())
	require.NotNil(t, response.Result)
	read := func(raw []byte, d *repb.Digest) string {
		if d == nil {
			return string(raw)
		}
		b, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		return string(b)
	}
	stdout, stderr := read(response.Result.StdoutRaw, response.Result.StdoutDigest), read(response.Result.StderrRaw, response.Result.StderrDigest)
	t.Logf("exit=%d stdout=%q stderr=%q", response.Result.ExitCode, stdout, stderr)
	return response.Result, stdout, stderr
}

func TestEstimatesAndBoundedChildren(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	// platform.go accepts cores/millicores, IEC sizes and numeric byte values.
	// Invalid values fall back to defaults; nonpositive requests are replaced by
	// tasksize defaults/minima. They are deliberately not a validation-error oracle.
	cases := []struct{ name, cpu, memory, disk string }{
		{"cores-IEC", "0.5", "64MB", "8MB"},
		{"millicores-bytes", "250m", "67108864", "8388608"},
		{"scientific", "2.5e-1", "6.4e7", "8e6"},
		{"minimum-clamping", "1m", "1", "1"},
		{"zero-defaults", "0", "0", "0"},
		{"negative-defaults", "-1", "-1", "-1"},
		{"malformed-defaults", "invalid", "invalid", "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, out, _ := execute(t, c, "printf estimates-ok", map[string]string{"EstimatedCPU": tc.cpu, "EstimatedMemory": tc.memory, "EstimatedFreeDiskBytes": tc.disk})
			require.Zero(t, r.ExitCode)
			require.Equal(t, "estimates-ok", out)
		})
	}
	t.Run("bounded-children", func(t *testing.T) {
		r, out, _ := execute(t, c, `i=0; while [ "$i" -lt 32 ]; do (sleep 1; printf 'child\n') & i=$((i+1)); done; wait`, map[string]string{"EstimatedCPU": "250m", "EstimatedMemory": "64MB"})
		require.Zero(t, r.ExitCode)
		require.Equal(t, 32, countLines(out, "child\n"))
	})
	// Small estimates do not impose hard limits in the bare provider. Allocate
	// only 8 MiB on disk, regardless of the scheduler's 1-byte disk request.
	t.Run("disk-estimate-is-hint", func(t *testing.T) {
		r, out, _ := execute(t, c, `dd if=/dev/zero of=scratch bs=1048576 count=8 2>/dev/null && wc -c < scratch`, map[string]string{"EstimatedFreeDiskBytes": "1"})
		require.Zero(t, r.ExitCode)
		require.Contains(t, out, "8388608")
	})
	for i := 0; i < 4; i++ {
		t.Run(fmt.Sprintf("concurrent-%d", i), func(t *testing.T) {
			t.Parallel()
			r, out, _ := execute(t, c, "sleep 1; printf concurrent-ok", map[string]string{"EstimatedCPU": "250m", "EstimatedMemory": "64MB"})
			require.Zero(t, r.ExitCode)
			require.Equal(t, "concurrent-ok", out)
		})
	}
}

func countLines(s, line string) int {
	n := 0
	for len(s) >= len(line) && s[:len(line)] == line {
		n++
		s = s[len(line):]
	}
	if s != "" {
		return -1
	}
	return n
}

func TestMemoryExhaustionAndRecovery(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	// RLIMIT_AS affects only this action's shell and descendants. This is an
	// allocator failure, not a cgroup OOM kill, and cannot exhaust the runner.
	r, _, stderr := execute(t, c, `ulimit -v 65536 || exit 90; exec awk 'BEGIN { s="xxxxxxxxxxxxxxxx"; for(i=0;i<23;i++) s=s s; print length(s) }'`, nil)
	require.NotEqual(t, int32(0), r.ExitCode)
	require.NotEqual(t, int32(90), r.ExitCode, "shell did not support RLIMIT_AS")
	require.Contains(t, stderr, "memory")
	r, out, _ := execute(t, c, "printf recovered", nil)
	require.Zero(t, r.ExitCode)
	require.Equal(t, "recovered", out)
}

func TestWorkspaceDiskFullAndRecovery(t *testing.T) {
	// Only executor workspaces use the 16 MiB tmpfs. CAS/file cache and the app's
	// storage remain on independent volumes, so ENOSPC cannot fill the runner.
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor},
		Config:     map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"root_directory": "/resource-workspace"}}},
		DockerArgs: map[harness.Component][]string{harness.Executor: {"--tmpfs", "/resource-workspace:rw,size=16m,mode=1777"}},
	})
	c := f.Clients(harness.App)
	r, _, stderr := execute(t, c, `exec dd if=/dev/zero of=scratch bs=1048576 count=32`, map[string]string{"EstimatedFreeDiskBytes": "8MB"})
	require.NotZero(t, r.ExitCode)
	require.Contains(t, stderr, "No space left on device")
	// Completion must remove the previous workspace and allow a fresh write.
	r, out, _ := execute(t, c, `dd if=/dev/zero of=scratch bs=1048576 count=4 2>/dev/null && printf recovered`, map[string]string{"EstimatedFreeDiskBytes": "4MB"})
	require.Zero(t, r.ExitCode)
	require.Equal(t, "recovered", out)
}
