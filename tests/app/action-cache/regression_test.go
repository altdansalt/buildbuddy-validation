package actioncache_test

import (
	"bytes"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sync"
	"testing"
)

func TestActionCacheRegression(t *testing.T) {
	ctx, c := setup(t)
	t.Run("InstancesAndOverwrite", func(t *testing.T) {
		a := action(t, ctx, c, "a", "same")
		b := action(t, ctx, c, "b", "same")
		require.Equal(t, a.Hash, b.Hash)
		require.Equal(t, a.SizeBytes, b.SizeBytes)
		put(t, ctx, c, "a", a, &repb.ActionResult{ExitCode: 1})
		_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: "b", ActionDigest: b})
		require.Equal(t, codes.NotFound, status.Code(err))
		put(t, ctx, c, "b", b, &repb.ActionResult{ExitCode: 2})
		put(t, ctx, c, "a", a, &repb.ActionResult{ExitCode: 3})
		for instance, want := range map[string]int32{"a": 3, "b": 2} {
			r, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: a})
			require.NoError(t, err)
			require.Equal(t, want, r.ExitCode)
		}
	})
	t.Run("InlineFilesAndEmptyFile", func(t *testing.T) {
		d := action(t, ctx, c, "", "inline")
		data := []byte("executable output")
		blob, err := c.Upload(ctx, "", data)
		require.NoError(t, err)
		put(t, ctx, c, "", d, &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out", Digest: blob, IsExecutable: true}, {Path: "empty", Digest: harness.Digest(nil)}}})
		r, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d, InlineOutputFiles: []string{"out", "empty"}})
		require.NoError(t, err)
		require.Len(t, r.OutputFiles, 2)
		require.Equal(t, data, r.OutputFiles[0].Contents)
		require.True(t, r.OutputFiles[0].IsExecutable)
		r, err = c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
		require.NoError(t, err)
		require.Empty(t, r.OutputFiles[0].Contents)
	})
	t.Run("MissingReferencesThenRepair", func(t *testing.T) {
		for _, kind := range []string{"file", "tree", "tree-root-file", "tree-child-file", "malformed-tree"} {
			t.Run(kind, func(t *testing.T) {
				d := action(t, ctx, c, "", kind)
				content := []byte("missing " + kind)
				file := harness.Digest(content)
				r := &repb.ActionResult{}
				switch kind {
				case "file":
					r.OutputFiles = []*repb.OutputFile{{Path: "out", Digest: file}}
				case "tree":
					r.OutputDirectories = []*repb.OutputDirectory{{Path: "dir", TreeDigest: file}}
				case "malformed-tree":
					td, err := c.Upload(ctx, "", []byte{0xff})
					require.NoError(t, err)
					r.OutputDirectories = []*repb.OutputDirectory{{Path: "dir", TreeDigest: td}}
				default:
					tree := &repb.Tree{Root: &repb.Directory{}}
					node := &repb.FileNode{Name: "out", Digest: file}
					if kind == "tree-root-file" {
						tree.Root.Files = []*repb.FileNode{node}
					} else {
						child := &repb.Directory{Files: []*repb.FileNode{node}}
						_, cd, err := harness.MarshalDigest(child)
						require.NoError(t, err)
						tree.Root.Directories = []*repb.DirectoryNode{{Name: "child", Digest: cd}}
						tree.Children = []*repb.Directory{child}
					}
					td, err := c.UploadProto(ctx, "", tree)
					require.NoError(t, err)
					r.OutputDirectories = []*repb.OutputDirectory{{Path: "dir", TreeDigest: td}}
				}
				put(t, ctx, c, "", d, r)
				_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
				require.Equal(t, codes.NotFound, status.Code(err))
				if kind == "file" || kind == "tree-root-file" || kind == "tree-child-file" {
					_, err = c.Upload(ctx, "", content)
					require.NoError(t, err)
					_, err = c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
					require.NoError(t, err)
				}
			})
		}
	})
	t.Run("InlineBudget", func(t *testing.T) {
		d := action(t, ctx, c, "", "large-inline")
		blob, err := c.Upload(ctx, "", bytes.Repeat([]byte("x"), 5<<20))
		require.NoError(t, err)
		put(t, ctx, c, "", d, &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out", Digest: blob}}})
		r, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d, InlineOutputFiles: []string{"out"}})
		require.NoError(t, err)
		require.Empty(t, r.OutputFiles[0].Contents)
		require.Equal(t, blob.Hash, r.OutputFiles[0].GetDigest().GetHash())
		require.Equal(t, blob.SizeBytes, r.OutputFiles[0].GetDigest().GetSizeBytes())
	})
	t.Run("ConcurrentOverwriteIsAtomic", func(t *testing.T) {
		d := action(t, ctx, c, "", "race")
		blobs := make([]*repb.Digest, 2)
		for i := range blobs {
			var err error
			blobs[i], err = c.Upload(ctx, "", []byte(fmt.Sprint(i)))
			require.NoError(t, err)
		}
		result := func(i int) *repb.ActionResult {
			return &repb.ActionResult{ExitCode: int32(i), OutputFiles: []*repb.OutputFile{{Path: "out", Digest: blobs[i]}}}
		}
		put(t, ctx, c, "", d, result(0))
		errs := make(chan error, 4)
		var wg sync.WaitGroup
		for worker := 0; worker < 4; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for n := 0; n < 20; n++ {
					if worker < 2 {
						_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: result(worker)})
						if err != nil {
							errs <- err
							return
						}
					} else {
						r, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
						if err != nil {
							errs <- err
							return
						}
						if r.ExitCode < 0 || r.ExitCode > 1 || len(r.OutputFiles) != 1 || r.OutputFiles[0].GetDigest().GetHash() != blobs[r.ExitCode].Hash {
							errs <- fmt.Errorf("torn result: %v", r)
							return
						}
					}
				}
			}(worker)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
	})
	t.Run("InvalidRequests", func(t *testing.T) {
		for _, d := range []*repb.Digest{nil, {Hash: "bad", SizeBytes: 1}, {Hash: harness.Digest([]byte("x")).Hash, SizeBytes: -1}} {
			_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: &repb.ActionResult{}})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
		}
		d := action(t, ctx, c, "", "nil-result")
		_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})
}
