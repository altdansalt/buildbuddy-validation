package inputtree_test

import (
	"context"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"sort"
	"strings"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

func TestInputTree(t *testing.T) {
	f := fixture(t)
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	blob, err := c.Upload(ctx, "input-tree", []byte("#!/bin/sh\nprintf materialized\n"))
	require.NoError(t, err)
	empty, err := c.UploadProto(ctx, "input-tree", &repb.Directory{})
	require.NoError(t, err)
	t.Run("symlinks", func(t *testing.T) {
		caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
		require.NoError(t, err)
		root := &repb.Directory{Files: []*repb.FileNode{{Name: "file", Digest: blob}}, Directories: []*repb.DirectoryNode{{Name: "empty", Digest: empty}}, Symlinks: []*repb.SymlinkNode{{Name: "dangling", Target: "missing"}, {Name: "dirlink", Target: "empty"}, {Name: "outside", Target: "../absent-input-tree-target"}, {Name: "relative", Target: "file"}, {Name: "self", Target: "self"}}}
		script := `test "$(readlink relative)" = file
cmp relative file
test -d dirlink
test -L dangling && test ! -e dangling
test "$(readlink outside)" = ../absent-input-tree-target
test -L self && test "$(readlink self)" = self`
		if caps.CacheCapabilities.SymlinkAbsolutePathStrategy == repb.SymlinkAbsolutePathStrategy_ALLOWED {
			root.Symlinks = append(root.Symlinks, &repb.SymlinkNode{Name: "absolute", Target: "/absent-input-tree-target"})
			script += "\ntest \"$(readlink absolute)\" = /absent-input-tree-target"
		}
		sort.Slice(root.Symlinks, func(i, j int) bool { return root.Symlinks[i].Name < root.Symlinks[j].Name })
		success(t, c, root, script)
	})
	t.Run("names-and-permissions", func(t *testing.T) {
		root := &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "empty", Digest: empty}}}
		script := "test -d empty\ntest -z \"$(ls -A empty)\"\n"
		names := []string{"-leading", "a b", "line\nbreak", "quote'\"$;[]", strings.Repeat("x", 255), "é-日本-😀"}
		for _, name := range names {
			root.Files = append(root.Files, &repb.FileNode{Name: name, Digest: blob})
			q := "'" + strings.ReplaceAll(name, "'", "'\"'\"'") + "'"
			script += "test -f ./" + q + "\ntest ! -x ./" + q + "\ncmp ./" + q + " ./plain\n"
		}
		root.Files = append(root.Files, &repb.FileNode{Name: "plain", Digest: blob}, &repb.FileNode{Name: "run", Digest: blob, IsExecutable: true})
		script += "test -x run\ntest \"$(./run)\" = materialized\n"
		sort.Slice(root.Files, func(i, j int) bool { return root.Files[i].Name < root.Files[j].Name })
		// Repeat to exercise warm file-cache fetches with the same digest in both modes.
		success(t, c, root, script)
		success(t, c, root, script)
	})
	t.Run("wide-and-deep-duplicate-digests", func(t *testing.T) {
		leaf := &repb.Directory{Files: []*repb.FileNode{{Name: "leaf", Digest: blob}}, Directories: []*repb.DirectoryNode{{Name: "empty", Digest: empty}}}
		leafDigest, err := c.UploadProto(ctx, "input-tree", leaf)
		require.NoError(t, err)
		d := leafDigest
		require.NoError(t, err)
		for i := 0; i < 128; i++ {
			d, err = c.UploadProto(ctx, "input-tree", &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "d", Digest: d}}})
			require.NoError(t, err)
		}
		root := &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "deep", Digest: d}, {Name: "duplicate-a", Digest: leafDigest}, {Name: "duplicate-b", Digest: leafDigest}}}
		for i := 0; i < 4096; i++ {
			root.Files = append(root.Files, &repb.FileNode{Name: fmt.Sprintf("f%04d", i), Digest: blob})
		}
		path := "deep/" + strings.Repeat("d/", 128)
		success(t, c, root, "test -f "+path+"leaf\ntest -d "+path+"empty\ncmp duplicate-a/leaf duplicate-b/leaf\nfor f in f*; do cmp \"$f\" "+path+"leaf; done\ntest \"$(find . -type f | wc -l)\" -eq 4099")
	})
}
