package platformprops_test

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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func platform(pairs ...string) *repb.Platform {
	p := &repb.Platform{}
	for i := 0; i < len(pairs); i += 2 {
		p.Properties = append(p.Properties, &repb.Platform_Property{Name: pairs[i], Value: pairs[i+1]})
	}
	sort.SliceStable(p.Properties, func(i, j int) bool {
		a, b := p.Properties[i], p.Properties[j]
		if a.Name == b.Name {
			return a.Value < b.Value
		}
		return a.Name < b.Name
	})
	return p
}
func base(extra ...string) *repb.Platform {
	return platform(append([]string{"OSFamily", "linux", "Arch", "amd64", "workload-isolation-type", "none"}, extra...)...)
}

// execute checks both legal error delivery paths: Execute's RPC status and the
// completed operation's ExecuteResponse status. Every request bypasses the AC.
func execute(t *testing.T, c *harness.Clients, actionPlatform, commandPlatform *repb.Platform) (codes.Code, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	instance := "platform-props"
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf platform-ok"}, Platform: commandPlatform})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Platform: actionPlatform, Timeout: durationpb.New(10 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	if err != nil {
		return status.Code(err), err.Error()
	}
	for {
		op, err := stream.Recv()
		if err != nil {
			require.NotEqual(t, io.EOF, err, "stream ended without result")
			return status.Code(err), err.Error()
		}
		if !op.Done {
			continue
		}
		if e := op.GetError(); e != nil {
			return codes.Code(e.Code), e.Message
		}
		response := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(response))
		if response.GetStatus().GetCode() != 0 {
			return codes.Code(response.Status.Code), response.Status.Message
		}
		require.NotNil(t, response.Result)
		require.Zero(t, response.Result.ExitCode)
		stdout := response.Result.StdoutRaw
		if d := response.Result.StdoutDigest; d != nil {
			stdout, err = c.Read(ctx, instance, d)
			require.NoError(t, err)
		}
		require.Equal(t, "platform-ok", string(stdout))
		return codes.OK, ""
	}
}

func TestPlatformProperties(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	cases := []struct {
		name            string
		action, command *repb.Platform
		code            codes.Code
		message         string
	}{
		{"command_fallback", nil, base(), codes.OK, ""},
		{"action_only", base(), nil, codes.OK, ""},
		{"action_replaces_conflicting_command", base(), platform("OSFamily", "not-an-os", "Arch", "not-an-arch", "workload-isolation-type", "invalid", "container-image", "docker://ignored"), codes.OK, ""},
		{"empty_action_replaces_command", platform(), base("container-image", "docker://ignored"), codes.OK, ""},
		{"case_insensitive_names_and_machine_values", platform("osfamily", "LiNuX", "ARCH", "AMD64", "WORKLOAD-ISOLATION-TYPE", "none"), nil, codes.OK, ""},
		{"trimmed_values", base("container-image", "  NoNe  "), nil, codes.OK, ""},
		{"unknown_extension_ignored", base("validation-unknown-property", "arbitrary"), nil, codes.OK, ""},
		{"identical_duplicate", base("container-image", "none", "container-image", "none"), nil, codes.OK, ""},
		{"duplicate_last_value", base("container-image", "docker://ignored", "container-image", "none"), nil, codes.OK, ""},
		{"bare_image_empty", base("container-image", ""), nil, codes.OK, ""},
		{"bare_image_none", base("container-image", "none"), nil, codes.OK, ""},
		{"bare_docker_image_rejected", base("container-image", "docker://alpine:3.20"), nil, codes.InvalidArgument, "Container images are not supported"},
		{"bare_malformed_image_rejected", base("container-image", "://"), nil, codes.InvalidArgument, "Container images are not supported"},
		{"unsupported_isolation", platform("OSFamily", "linux", "Arch", "amd64", "workload-isolation-type", "invalid"), nil, codes.InvalidArgument, ""},
		{"isolation_value_case_sensitive", platform("OSFamily", "linux", "Arch", "amd64", "workload-isolation-type", "NONE"), nil, codes.InvalidArgument, ""},
		{"invalid_duration", base("default-timeout", "nonsense"), nil, codes.InvalidArgument, "invalid duration"},
		{"invalid_prefetch_mode", base("vfs-prefetch-mode", "invalid"), nil, codes.InvalidArgument, "vfs-prefetch-mode"},
		{"invalid_base64", base("env-overrides-base64", "%%%"), nil, codes.InvalidArgument, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := execute(t, c, tc.action, tc.command)
			t.Logf("status=%s message=%s", code, msg)
			require.Equal(t, tc.code, code, fmt.Sprintf("message: %s", msg))
			if tc.message != "" {
				require.Contains(t, msg, tc.message)
			}
		})
	}
	// A valid execution after all rejected requests checks continued service health.
	t.Run("healthy_after_rejections", func(t *testing.T) { code, msg := execute(t, c, base(), nil); require.Equal(t, codes.OK, code, msg) })
}
