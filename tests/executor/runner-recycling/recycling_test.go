package recycling_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

func fixture(t *testing.T) *harness.Clients {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	return f.Clients(harness.App)
}

func execute(t *testing.T, c *harness.Clients, instance, key, script string, env bool, timeout time.Duration) (*repb.ExecuteResponse, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	cmd := &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "Arch", Value: "amd64"}, {Name: "OSFamily", Value: "linux"}, {Name: "recycle-runner", Value: "true"}, {Name: "runner-recycling-test-key", Value: key}, {Name: "workload-isolation-type", Value: "none"}}}}
	if env {
		cmd.EnvironmentVariables = []*repb.Command_EnvironmentVariable{{Name: "RECYCLING_SECRET", Value: "first-action-only"}}
	}
	cd, err := c.UploadProto(ctx, instance, cmd)
	require.NoError(t, err)
	ad, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cd, InputRootDigest: root, Timeout: durationpb.New(timeout), DoNotCache: true})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: ad, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
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
		require.Nil(t, op.GetError())
		response = &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(response))
		break
	}
	require.NotNil(t, response)
	stdout := response.GetResult().GetStdoutRaw()
	if d := response.GetResult().GetStdoutDigest(); d != nil {
		stdout, err = c.Read(ctx, instance, d)
		require.NoError(t, err)
	}
	t.Logf("instance=%s key=%s status=%d exit=%d stdout=%q", instance, key, response.GetStatus().GetCode(), response.GetResult().GetExitCode(), stdout)
	// Completion is published before asynchronous pooling; allow that finalization to finish.
	time.Sleep(time.Second)
	return response, strings.TrimSpace(string(stdout))
}
func ok(t *testing.T, r *repb.ExecuteResponse) {
	t.Helper()
	require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
	require.Zero(t, r.GetResult().GetExitCode())
}

func TestRegression(t *testing.T) {
	c := fixture(t)
	r, path := execute(t, c, "recycling", "a", "pwd; mkdir -p stale/nested; touch stale/nested/file; ln -s stale alias; export LOCAL_ONLY=secret", true, 30*time.Second)
	ok(t, r)
	r, next := execute(t, c, "recycling", "a", `pwd; test ! -e stale && test ! -L alias && test -z "${RECYCLING_SECRET:-}" && test -z "${LOCAL_ONLY:-}"`, false, 30*time.Second)
	ok(t, r)
	require.Equal(t, path, next, "must exercise the same recycled workspace")
	r, next = execute(t, c, "recycling", "a", "pwd; touch failure-residue; exit 17", false, 30*time.Second)
	require.Zero(t, r.GetStatus().GetCode())
	require.EqualValues(t, 17, r.GetResult().GetExitCode())
	require.Equal(t, path, next)
	r, _ = execute(t, c, "recycling", "a", "test ! -e failure-residue", false, 30*time.Second)
	ok(t, r)
	r, other := execute(t, c, "recycling", "b", "pwd", false, 30*time.Second)
	ok(t, r)
	require.NotEqual(t, path, other, "different platform must not reuse runner")
	r, other = execute(t, c, "other-instance", "a", "pwd", false, 30*time.Second)
	ok(t, r)
	require.NotEqual(t, path, other, "different instance must not reuse runner")
	r, old := execute(t, c, "timeout", "a", `pwd; touch timeout-residue; sleep 60 & child=$!; printf "%s\n" "$child"; wait "$child"`, false, 2*time.Second)
	require.EqualValues(t, 4, r.GetStatus().GetCode(), "deadline status")
	lines := strings.Split(old, "\n")
	require.Len(t, lines, 2)
	old = lines[0]
	r, newPath := execute(t, c, "timeout", "a", fmt.Sprintf("pwd; test ! -e timeout-residue; test ! -d '%s'; ! kill -0 %s 2>/dev/null", old, lines[1]), false, 30*time.Second)
	ok(t, r)
	require.NotEqual(t, old, newPath, "timed-out runner must be removed")
}

func TestTmpLeak(t *testing.T) {
	c := fixture(t)
	r, path := execute(t, c, "tmp-leak", "a", `pwd; test -n "$TMPDIR"; printf 'private-first-action-data' > "$TMPDIR/recycling-secret"`, false, 30*time.Second)
	ok(t, r)
	r, next := execute(t, c, "tmp-leak", "a", `pwd; if test -e "$TMPDIR/recycling-secret"; then printf 'LEAK: '; cat "$TMPDIR/recycling-secret"; exit 42; fi`, false, 30*time.Second)
	require.True(t, strings.HasPrefix(next, path), "must exercise same runner: first=%q second=%q", path, next)
	ok(t, r)
}
