package randexec_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func uploadCase(t *testing.T, ctx context.Context, c *harness.Clients, tc testCase) *repb.Action {
	t.Helper()
	input, err := c.Upload(ctx, "round2", []byte(tc.Input))
	require.NoError(t, err)
	root, err := c.UploadProto(ctx, "round2", &repb.Directory{Files: []*repb.FileNode{{Name: "input.txt", Digest: input}}})
	require.NoError(t, err)
	cmd, err := c.UploadProto(ctx, "round2", tc.Command)
	require.NoError(t, err)
	return &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(20 * time.Second)}
}

func TestRound2ConcurrentProperties(t *testing.T) {
	for _, isolation := range []string{"none", "docker"} {
		t.Run(isolation, func(t *testing.T) {
			var f *harness.Fixture
			if isolation == "docker" {
				f = dockerFixture(t)
			} else {
				f = bareFixture(t)
			}
			c := f.Clients(harness.App)
			r := rand.New(rand.NewSource(*seed))
			// Generate before starting concurrent subtests: the PRNG is deterministic and race-free.
			for i := 0; i < 8; i++ {
				tc := generate(r, i*5) // Concurrent cases are successful and cacheable; sequential coverage includes nonzero exits.
				tc.Command.OutputFiles = nil
				tc.Command.OutputDirectories = nil
				tc.Command.OutputPaths = []string{"dangling", "file-link", "result.txt", "tree", "tree/nested/copy"}
				tc.Command.Arguments[2] += "ln -s tree/nested/copy file-link\nln -s absent dangling\nln -s nested/copy tree/link\nln -s empty tree/dir-link\n"
				if isolation == "docker" {
					tc.Command.Platform.Properties = []*repb.Platform_Property{{Name: "Arch", Value: "amd64"}, {Name: "OSFamily", Value: "linux"}, {Name: "container-image", Value: "docker://mirror.gcr.io/library/alpine:3.21.3"}, {Name: "workload-isolation-type", Value: "docker"}}
				}
				t.Run(fmt.Sprintf("seed_%d_case_%d", *seed, i), func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					t.Logf("input=%q command=%s", tc.Input, tc.Command)
					expected := reference(t, ctx, tc)
					a := uploadCase(t, ctx, c, tc)
					d, err := c.UploadProto(ctx, "round2", a)
					require.NoError(t, err)
					for repeat := 0; repeat < 2; repeat++ {
						result := execute(t, ctx, c, "round2", d, true)
						require.False(t, result.CachedResult)
						require.Equal(t, expected, normalized(t, ctx, c, "round2", result.Result))
					}
					hit := execute(t, ctx, c, "round2", d, false)
					require.True(t, hit.CachedResult)
					require.Equal(t, expected, normalized(t, ctx, c, "round2", hit.Result))
				})
			}
		})
	}
}

// Identical observable outputs are valid for different actions. Instead of
// asserting unequal output bytes, place a sentinel under only the original key.
// Every single-field variant must miss it, then retain its own cached result.
func TestActionDigestFieldSeparation(t *testing.T) {
	f := bareFixture(t)
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tc := generate(rand.New(rand.NewSource(*seed)), 0)
	base := uploadCase(t, ctx, c, tc)
	d, err := c.UploadProto(ctx, "round2", base)
	require.NoError(t, err)
	sentinel := &repb.ActionResult{ExitCode: 0, StdoutRaw: []byte("digest-key-sentinel")}
	_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: "round2", ActionDigest: d, ActionResult: sentinel, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	variants := map[string]func(*repb.Action){
		"command_digest": func(a *repb.Action) {
			cmd := proto.Clone(tc.Command).(*repb.Command)
			cmd.Arguments = append(cmd.Arguments, "unused-argument")
			a.CommandDigest, err = c.UploadProto(ctx, "round2", cmd)
			require.NoError(t, err)
		},
		"input_root_digest": func(a *repb.Action) {
			a.InputRootDigest, err = c.UploadProto(ctx, "round2", &repb.Directory{Files: []*repb.FileNode{{Name: "input.txt", Digest: harness.Digest([]byte(tc.Input))}}, Directories: []*repb.DirectoryNode{{Name: "unused", Digest: emptyDirectory(t, ctx, c)}}})
			require.NoError(t, err)
		},
		"timeout":      func(a *repb.Action) { a.Timeout = durationpb.New(21 * time.Second) },
		"do_not_cache": func(a *repb.Action) { a.DoNotCache = true },
		"salt":         func(a *repb.Action) { a.Salt = []byte("round2-salt") },
		"platform":     func(a *repb.Action) { a.Platform = proto.Clone(tc.Command.Platform).(*repb.Platform) },
	}
	for _, name := range []string{"command_digest", "input_root_digest", "timeout", "do_not_cache", "salt", "platform"} {
		mutate := variants[name]
		t.Run(name, func(t *testing.T) {
			a := proto.Clone(base).(*repb.Action)
			mutate(a)
			key, err := c.UploadProto(ctx, "round2", a)
			require.NoError(t, err)
			require.NotEqual(t, d.Hash, key.Hash)
			got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: "round2", ActionDigest: key, DigestFunction: repb.DigestFunction_SHA256})
			require.Equal(t, codes.NotFound, status.Code(err), "variant inherited sentinel: %v", got)
			result := execute(t, ctx, c, "round2", key, false)
			require.False(t, result.CachedResult)
			require.Equal(t, reference(t, ctx, tc), normalized(t, ctx, c, "round2", result.Result))
			if !a.DoNotCache {
				hit := execute(t, ctx, c, "round2", key, false)
				require.True(t, hit.CachedResult)
				require.Equal(t, normalized(t, ctx, c, "round2", result.Result), normalized(t, ctx, c, "round2", hit.Result))
			} else {
				_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: "round2", ActionDigest: key, DigestFunction: repb.DigestFunction_SHA256})
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			original, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: "round2", ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Equal(t, sentinel.StdoutRaw, original.StdoutRaw)
		})
	}
}
func emptyDirectory(t *testing.T, ctx context.Context, c *harness.Clients) *repb.Digest {
	t.Helper()
	d, err := c.UploadProto(ctx, "round2", &repb.Directory{})
	require.NoError(t, err)
	return d
}
