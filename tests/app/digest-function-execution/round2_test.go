package digestexecution_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func checkLinks(t *testing.T, ctx context.Context, c *harness.Clients, df repb.DigestFunction_Value, r *repb.ActionResult, payload []byte) {
	t.Helper()
	checkResult(t, ctx, c, df, r, payload)
	links := map[string]string{}
	for _, link := range r.OutputSymlinks {
		links[link.Path] = link.Target
	}
	require.Len(t, r.OutputSymlinks, 3)
	require.Equal(t, map[string]string{"file-link": "out", "dir-link": "tree", "dangling-link": "missing"}, links)
	// REAPI permits these deprecated fields for v2.0 compatibility.
	for _, link := range append(append([]*repb.OutputSymlink{}, r.OutputFileSymlinks...), r.OutputDirectorySymlinks...) {
		require.Contains(t, links, link.Path)
		require.Equal(t, links[link.Path], link.Target)
	}
	tree := new(repb.Tree)
	require.NoError(t, proto.Unmarshal(read(t, ctx, c, df, r.OutputDirectories[0].TreeDigest), tree))
	require.Len(t, tree.Children[0].Symlinks, 2)
	for i, want := range []*repb.SymlinkNode{{Name: "copy-link", Target: "copy"}, {Name: "dangling", Target: "absent"}} {
		require.True(t, proto.Equal(want, tree.Children[0].Symlinks[i]), "nested symlink %d: %v", i, tree.Children[0].Symlinks[i])
	}
}

// Parallel subtests share one executor and cache namespace. Upload all actions
// before releasing a wave so that the scheduling order is independent of upload
// latency. The same payload and command exercise SHA256/BLAKE3 hash-length
// ambiguity; distinct seeded cases detect accidental result cross-contamination.
func TestConcurrentDigestFunctionsAndSymlinks(t *testing.T) {
	require.GreaterOrEqual(t, *iterations, 1)
	require.LessOrEqual(t, *iterations, 100)
	for _, viaProxy := range []bool{false, true} {
		t.Run(fmt.Sprintf("executor-cache-proxy=%t", viaProxy), func(t *testing.T) {
			t.Logf("seed=%d waves=%d", *seed, *iterations)
			var config map[harness.Component]harness.Config
			if viaProxy {
				config = map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"cache_target": "grpc://cacheproxy:1985", "cache_target_traffic_percent": 100}}}
			}
			f := startWithConfig(t, config)
			app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			if viaProxy {
				// The harness starts Executor before CacheProxy. gRPC may retain the
				// initial DNS failure until its reconnect timer fires. Retry only that
				// exact startup failure before beginning the measured concurrent waves.
				warmCtx, warmCancel := context.WithTimeout(ctx, time.Minute)
				defer warmCancel()
				d := action(t, warmCtx, proxy, repb.DigestFunction_SHA256, []byte("warmup"), *seed-1)
				for {
					r, err := execute(warmCtx, app, repb.DigestFunction_SHA256, d, true)
					require.NoError(t, err)
					if r.GetStatus().GetCode() == int32(codes.Unavailable) && strings.Contains(r.GetStatus().GetMessage(), "lookup cacheproxy") && strings.Contains(r.GetStatus().GetMessage(), "no such host") {
						t.Log("waiting for executor cache connection after proxy startup")
						select {
						case <-warmCtx.Done():
							t.Fatal(warmCtx.Err())
						case <-time.After(time.Second):
						}
						continue
					}
					require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
					checkResult(t, warmCtx, proxy, repb.DigestFunction_SHA256, r.Result, []byte("warmup"))
					break
				}
			}
			rng := rand.New(rand.NewSource(*seed))
			for wave := 0; wave < *iterations; wave++ {
				payload := make([]byte, rng.Intn(8193))
				rng.Read(payload)
				if wave == 0 {
					payload = []byte{}
				}
				actions := make(map[repb.DigestFunction_Value]*repb.Digest)
				for _, df := range functions {
					actions[df] = actionCommand(t, ctx, proxy, df, payload, *seed+int64(wave), "mkdir -p tree/sub; cat in > out; cat in > tree/sub/copy; ln -s copy tree/sub/copy-link; ln -s absent tree/sub/dangling; ln -s out file-link; ln -s tree dir-link; ln -s missing dangling-link; sleep 0.2; printf 'stdout'; printf 'stderr' >&2", []string{"dangling-link", "dir-link", "file-link", "out", "tree"})
				}
				t.Run(fmt.Sprintf("wave=%d", wave), func(t *testing.T) {
					for _, df := range functions {
						df := df
						t.Run(df.String(), func(t *testing.T) {
							t.Parallel()
							d := actions[df]
							r, err := execute(ctx, app, df, d, true)
							require.NoError(t, err)
							require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
							for _, c := range []*harness.Clients{app, proxy} {
								checkLinks(t, ctx, c, df, r.Result, payload)
								ac, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, DigestFunction: df, ActionDigest: d})
								require.NoError(t, err)
								checkLinks(t, ctx, c, df, ac, payload)
								for _, other := range functions {
									if other == df {
										continue
									}
									_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, DigestFunction: other, ActionDigest: d})
									require.Contains(t, []codes.Code{codes.NotFound, codes.InvalidArgument}, status.Code(err))
								}
							}
							cached, err := execute(ctx, app, df, d, false)
							require.NoError(t, err)
							require.Zero(t, cached.GetStatus().GetCode())
							require.True(t, cached.CachedResult)
							checkLinks(t, ctx, proxy, df, cached.Result, payload)
						})
					}
				})
			}
		})
	}
}
