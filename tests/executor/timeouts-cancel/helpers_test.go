package timeouts_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

const instance = "timeouts-cancel"

func upload(t *testing.T, ctx context.Context, c *harness.Clients, script string, timeout time.Duration, props ...*repb.Platform_Property) *repb.Digest {
	t.Helper()
	return uploadCommand(t, ctx, c, script, timeout, nil, props...)
}

func uploadTree(t *testing.T, ctx context.Context, c *harness.Clients, script string, timeout time.Duration, props ...*repb.Platform_Property) *repb.Digest {
	t.Helper()
	return uploadCommand(t, ctx, c, script, timeout, []string{"partial.txt"}, props...)
}

func uploadCommand(t *testing.T, ctx context.Context, c *harness.Clients, script string, timeout time.Duration, outputs []string, props ...*repb.Platform_Property) *repb.Digest {
	t.Helper()
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	platform := []*repb.Platform_Property{{Name: "Arch", Value: "amd64"}, {Name: "OSFamily", Value: "linux"}, {Name: "workload-isolation-type", Value: "none"}}
	for _, prop := range props {
		found := false
		for i, existing := range platform {
			if existing.Name == prop.Name {
				platform[i] = prop
				found = true
				break
			}
		}
		if !found {
			platform = append(platform, prop)
		}
	}
	sort.Slice(platform, func(i, j int) bool { return platform[i].Name < platform[j].Name })
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: platform}, OutputPaths: outputs})
	require.NoError(t, err)
	action := &repb.Action{CommandDigest: cmd, InputRootDigest: root, DoNotCache: true}
	if timeout != 0 {
		action.Timeout = durationpb.New(timeout)
	}
	d, err := c.UploadProto(ctx, instance, action)
	require.NoError(t, err)
	return d
}

func execute(t *testing.T, ctx context.Context, c *harness.Clients, d *repb.Digest) *repb.ExecuteResponse {
	t.Helper()
	s, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: true})
	require.NoError(t, err)
	for {
		op, err := s.Recv()
		require.NoError(t, err, "Execute must deliver a terminal operation")
		require.Nil(t, op.GetError(), "REAPI forbids Operation.error")
		if op.Done {
			r := &repb.ExecuteResponse{}
			require.NoError(t, op.GetResponse().UnmarshalTo(r))
			t.Logf("operation %s: status=%v exit=%d", op.Name, r.GetStatus(), r.GetResult().GetExitCode())
			return r
		}
	}
}

func stdout(t *testing.T, ctx context.Context, c *harness.Clients, r *repb.ExecuteResponse) string {
	t.Helper()
	if r.GetResult().GetStdoutDigest() != nil {
		b, err := c.Read(ctx, instance, r.Result.StdoutDigest)
		require.NoError(t, err)
		return string(b)
	}
	return string(r.GetResult().GetStdoutRaw())
}

// Register before Start so this runs after the harness preserves container logs.
func startFixture(t *testing.T, opts harness.Options) *harness.Fixture {
	t.Helper()
	var f *harness.Fixture
	t.Cleanup(func() {
		if f == nil {
			return
		}
		for _, component := range []harness.Component{harness.App, harness.Executor} {
			ep := f.Endpoint(component)
			b, err := os.ReadFile(filepath.Join(f.OutputDir, ep.Container+".logs.txt"))
			require.NoError(t, err)
			logs := string(b)
			require.NotContains(t, logs, "panic:", "%s container panicked", component)
			require.NotContains(t, logs, "fatal error:", "%s runtime failed", component)
			for _, line := range strings.Split(logs, "\n") {
				if strings.Contains(line, "graceful termination signal") || strings.Contains(line, "Command execution returned error") || strings.Contains(line, "Failed to kill process tree") || strings.Contains(line, "Created invocation") || strings.Contains(line, "Cancelling") || strings.Contains(line, "Discarding anonymous") || strings.Contains(line, "Voiding EventChannel") {
					t.Logf("%s: %s", component, line)
				}
			}
		}
	})
	f = harness.Start(t, opts)
	return f
}
