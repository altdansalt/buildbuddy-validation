package actioncache_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Upload the Command and Action first, as required by REAPI UpdateActionResult.
func action(t *testing.T, ctx context.Context, c *harness.Clients, instance string) *repb.Digest {
	t.Helper()
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"true"}})
	require.NoError(t, err)
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	d, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root})
	require.NoError(t, err)
	return d
}

func TestActionCache(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	clients := []*harness.Clients{app, proxy}
	get := func(c *harness.Clients, instance string, d *repb.Digest) *repb.ActionResult {
		t.Helper()
		got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		return got
	}
	update := func(c *harness.Clients, instance string, d *repb.Digest, result *repb.ActionResult) *repb.ActionResult {
		t.Helper()
		got, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, ActionResult: result, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		return got
	}
	t.Run("missing_and_invalid_arguments", func(t *testing.T) {
		for _, c := range clients {
			_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: harness.Digest([]byte("absent"))})
			require.Equal(t, codes.NotFound, status.Code(err))
			for _, d := range []*repb.Digest{nil, {Hash: "bad", SizeBytes: 1}, {Hash: harness.Digest(nil).Hash, SizeBytes: -1}} {
				_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
				require.Equal(t, codes.InvalidArgument, status.Code(err), "digest %v: %v", d, err)
				_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: &repb.ActionResult{}})
				require.Equal(t, codes.InvalidArgument, status.Code(err))
			}
			_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: harness.Digest([]byte("nil-result"))})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
		}
	})
	t.Run("instances_and_revalidation", func(t *testing.T) {
		instances := []string{"", "team/project", "team/project-other"}
		for i, instance := range instances {
			d := action(t, ctx, app, instance)
			want := update(proxy, instance, d, &repb.ActionResult{StdoutRaw: []byte(fmt.Sprintf("instance %d", i))})
			for _, c := range clients {
				require.True(t, proto.Equal(want, get(c, instance, d)))
			}
		}
		for i, instance := range instances {
			d := action(t, ctx, app, instance)
			require.Equal(t, []byte(fmt.Sprintf("instance %d", i)), get(proxy, instance, d).StdoutRaw)
			// Warm multiple request variants before replacing the authoritative entry.
			for _, inline := range []bool{false, true} {
				_, err := proxy.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, InlineStdout: inline})
				require.NoError(t, err)
			}
			for n := 0; n < 4; n++ {
				writer := clients[n%2]
				want := update(writer, instance, d, &repb.ActionResult{StdoutRaw: []byte(fmt.Sprintf("replacement %d", n)), ExitCode: int32(n)})
				for _, c := range clients {
					require.True(t, proto.Equal(want, get(c, instance, d)), "instance %q update %d", instance, n)
				}
			}
		}
	})
	t.Run("missing_outputs_and_recovery", func(t *testing.T) {
		instance := "output-validation"
		d := action(t, ctx, app, instance)
		want := update(proxy, instance, d, &repb.ActionResult{StdoutRaw: []byte("valid")})
		require.True(t, proto.Equal(want, get(proxy, instance, d)))
		contents := []byte("output initially missing from CAS")
		missing := harness.Digest(contents)
		update(app, instance, d, &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out", Digest: missing}}})
		for _, c := range clients {
			_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d})
			require.Equal(t, codes.NotFound, status.Code(err))
		}
		_, err := app.Upload(ctx, instance, contents)
		require.NoError(t, err)
		for _, c := range clients {
			for n := 0; n < 2; n++ {
				got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, InlineOutputFiles: []string{"out"}})
				require.NoError(t, err)
				require.Len(t, got.OutputFiles, 1)
				require.Equal(t, contents, got.OutputFiles[0].Contents)
			}
		}
	})
}
