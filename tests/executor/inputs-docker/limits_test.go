package inputtree_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestInputTreeLimits(t *testing.T) {
	f := fixture(t)
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
	t.Run("65536-files-256-directories", func(t *testing.T) {
		root := &repb.Directory{}
		for i := 0; i < 256; i++ {
			root.Directories = append(root.Directories, &repb.DirectoryNode{Name: fmt.Sprintf("d%03d", i), Digest: ld})
		}
		// Check all names and sizes without spawning a process per file; sample
		// contents in every directory. This keeps command runtime bounded.
		script := "test \"$(find . -type f | wc -l)\" -eq 65536\ntest \"$(find . -type f ! -size 15c | wc -l)\" -eq 0\nfor d in d*; do test \"$(cat \"$d/f000\")\" = shared-content; test \"$(cat \"$d/f255\")\" = shared-content; done"
		success(t, c, root, script)
	})
	t.Run("optional-node-properties", func(t *testing.T) {
		props := &repb.NodeProperties{Mtime: timestamppb.New(time.Unix(946684800, 123456789)), UnixMode: wrapperspb.UInt32(0600)}
		dir, err := c.UploadProto(ctx, "input-tree", &repb.Directory{NodeProperties: &repb.NodeProperties{Mtime: props.Mtime, UnixMode: wrapperspb.UInt32(0700)}})
		require.NoError(t, err)
		// Verify that the metadata-only Directory really exists in fixture CAS.
		stored, err := c.Read(ctx, "input-tree", dir)
		require.NoError(t, err)
		require.Len(t, stored, int(dir.SizeBytes))
		require.NotEmpty(t, stored)
		root := &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "dir", Digest: dir}}}
		success(t, c, root, "test -d dir\ntest -z \"$(ls -A dir)\"")
	})
}
