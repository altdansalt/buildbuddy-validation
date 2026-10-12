package inputtree_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestInputTreeRound2(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	blob, err := c.Upload(ctx, "input-tree", []byte("shared-content\n"))
	require.NoError(t, err)
	leaf := &repb.Directory{}
	for i := 0; i < 256; i++ {
		leaf.Files = append(leaf.Files, &repb.FileNode{Name: fmt.Sprintf("f%03d", i), Digest: blob})
	}
	ld, err := c.UploadProto(ctx, "input-tree", leaf)
	require.NoError(t, err)

	t.Run("concurrent-cold-and-warm", func(t *testing.T) {
		// Gate immediately before Execute, after every action is uploaded. Distinct
		// scripts prevent action merging while identical roots share every CAS node.
		for wave := 0; wave < 2; wave++ {
			var ready sync.WaitGroup
			ready.Add(8)
			gate := make(chan struct{})
			go func() { ready.Wait(); close(gate) }()
			t.Run(fmt.Sprintf("wave-%d", wave), func(t *testing.T) {
				for i := 0; i < 8; i++ {
					t.Run(fmt.Sprintf("action-%d", i), func(t *testing.T) {
						t.Parallel()
						root := &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "shared", Digest: ld}}}
						if i >= 4 {
							root.Files = []*repb.FileNode{{Name: "private", Digest: blob, IsExecutable: i%2 == 0}}
						}
						script := fmt.Sprintf("set -eu\n# wave %d action %d\n", wave, i)
						script += "for f in shared/*; do test \"$(cat \"$f\")\" = shared-content; test ! -x \"$f\"; done\ntest \"$(find shared -type f | wc -l)\" -eq 256\n"
						if i >= 4 {
							script += "test \"$(cat private)\" = shared-content\n"
							if i%2 == 0 {
								script += "test -x private\n"
							} else {
								script += "test ! -x private\n"
							}
						} else {
							script += "test ! -e private\n"
						}
						// Inputs are hardlinked and must not be modified in place. Replace the
						// directory entry before writing to keep cached input inodes immutable.
						script += "rm shared/f000\nprintf changed > shared/f000\ntest \"$(cat shared/f001)\" = shared-content\nsleep 1\n"
						r := executeAfter(t, c, root, script, func() { ready.Done(); <-gate })
						require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
						require.NotNil(t, r.Result)
						require.Zero(t, r.Result.ExitCode, "%v", r.Result)
					})
				}
			})
		}
	})
	t.Run("optional-node-properties-nonempty-directory", func(t *testing.T) {
		props := &repb.NodeProperties{Mtime: timestamppb.New(time.Unix(946684800, 123456789)), UnixMode: wrapperspb.UInt32(0600)}
		dir, err := c.UploadProto(ctx, "input-tree", &repb.Directory{Files: []*repb.FileNode{{Name: "child", Digest: blob}}, NodeProperties: &repb.NodeProperties{Mtime: props.Mtime, UnixMode: wrapperspb.UInt32(0700)}})
		require.NoError(t, err)
		root := &repb.Directory{Files: []*repb.FileNode{{Name: "metadata", Digest: blob, NodeProperties: props}, {Name: "plain", Digest: blob}, {Name: "run", Digest: blob, IsExecutable: true, NodeProperties: props}}, Directories: []*repb.DirectoryNode{{Name: "dir", Digest: dir}}}
		for pass := 0; pass < 2; pass++ {
			success(t, c, root, fmt.Sprintf("# metadata pass %d\n", pass)+"test ! -x metadata\ntest -x run\ncmp metadata plain\ncmp dir/child plain")
		}
	})

}
