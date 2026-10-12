package outputs_test

import (
	"bytes"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestOutputCollection(t *testing.T) {
	r := start(t)
	t.Run("modern-files-missing-exec-empty-mtime", func(t *testing.T) {
		cmd := shell("printf content > executable; chmod 755 executable; touch -t 200001010000 executable; : > empty; printf ignored > undeclared")
		cmd.OutputPaths = []string{"empty", "executable", "missing"}
		result := success(t, r.execute(t, cmd, nil))
		require.Len(t, result.OutputFiles, 2)
		require.Empty(t, result.OutputDirectories)
		files := map[string]*repb.OutputFile{}
		for _, f := range result.OutputFiles {
			files[f.Path] = f
		}
		require.Equal(t, []byte("content"), r.read(t, files["executable"].Digest))
		require.True(t, files["executable"].IsExecutable)
		require.Empty(t, r.read(t, files["empty"].Digest))
		require.False(t, files["empty"].IsExecutable)
		// With no requested node properties, mtime must not affect the content digest.
		require.Equal(t, harness.Digest([]byte("content")), files["executable"].Digest)
	})
	t.Run("empty-and-missing-modern-directories", func(t *testing.T) {
		cmd := shell("mkdir emptydir; test ! -e missingdir")
		cmd.OutputPaths = []string{"emptydir", "missingdir"}
		result := success(t, r.execute(t, cmd, nil))
		require.Len(t, result.OutputDirectories, 1)
		require.Equal(t, "emptydir", result.OutputDirectories[0].Path)
		tree := &repb.Tree{}
		require.NoError(t, proto.Unmarshal(r.read(t, result.OutputDirectories[0].TreeDigest), tree))
		require.Empty(t, tree.Root.Files)
		require.Empty(t, tree.Root.Directories)
		require.Empty(t, tree.Root.Symlinks)
	})
	t.Run("no-declared-outputs", func(t *testing.T) {
		result := success(t, r.execute(t, shell("printf discarded > undeclared; mkdir unlisted"), nil))
		require.Empty(t, result.OutputFiles)
		require.Empty(t, result.OutputDirectories)
		require.Empty(t, result.OutputSymlinks)
	})

	t.Run("legacy-file", func(t *testing.T) {
		cmd := shell("printf legacy > out")
		cmd.OutputFiles = []string{"missing", "out"}
		result := success(t, r.execute(t, cmd, nil))
		require.Len(t, result.OutputFiles, 1)
		require.Equal(t, "out", result.OutputFiles[0].Path)
		require.Equal(t, []byte("legacy"), r.read(t, result.OutputFiles[0].Digest))
	})
	for _, legacy := range []bool{false, true} {
		name := "modern-tree"
		if legacy {
			name = "legacy-tree"
		}
		t.Run(name, func(t *testing.T) {
			cmd := shell("mkdir -p tree/empty tree/nested; printf nested > tree/nested/file; chmod 755 tree/nested/file; ln -s nested/file tree/link; ln -s absent tree/dangling")
			if legacy {
				cmd.OutputDirectories = []string{"tree"}
			} else {
				cmd.OutputPaths = []string{"tree"}
			}
			result := success(t, r.execute(t, cmd, nil))
			require.Empty(t, result.OutputFiles)
			require.Len(t, result.OutputDirectories, 1)
			tree := &repb.Tree{}
			require.NoError(t, proto.Unmarshal(r.read(t, result.OutputDirectories[0].TreeDigest), tree))
			require.Len(t, tree.Root.Directories, 2)
			require.Len(t, tree.Root.Symlinks, 2)
			require.Equal(t, "dangling", tree.Root.Symlinks[0].Name)
			require.Equal(t, "absent", tree.Root.Symlinks[0].Target)
			require.Equal(t, "link", tree.Root.Symlinks[1].Name)
			require.Equal(t, "nested/file", tree.Root.Symlinks[1].Target)
			dirs := map[string]*repb.Directory{}
			for _, d := range tree.Children {
				_, digest, err := harness.MarshalDigest(d)
				require.NoError(t, err)
				dirs[digest.Hash] = d
			}
			require.Empty(t, dirs[tree.Root.Directories[0].Digest.Hash].Files)
			nested := dirs[tree.Root.Directories[1].Digest.Hash]
			require.Len(t, nested.Files, 1)
			require.True(t, nested.Files[0].IsExecutable)
			require.Equal(t, []byte("nested"), r.read(t, nested.Files[0].Digest))
		})
	}
	t.Run("symlinks", func(t *testing.T) {
		cmd := shell("printf target > target; mkdir targetdir; ln -s target filelink; ln -s targetdir dirlink; ln -s absent dangling")
		cmd.OutputPaths = []string{"dangling", "dirlink", "filelink"}
		result := success(t, r.execute(t, cmd, nil))
		require.Len(t, result.OutputSymlinks, 3)
		require.Empty(t, result.OutputFiles)
		require.Empty(t, result.OutputDirectories)
		links := map[string]string{}
		for _, s := range result.OutputSymlinks {
			links[s.Path] = s.Target
		}
		require.Equal(t, map[string]string{"dangling": "absent", "dirlink": "targetdir", "filelink": "target"}, links)
	})
	t.Run("overlapping-input", func(t *testing.T) {
		d, err := r.c.Upload(r.ctx, instance, []byte("before"))
		require.NoError(t, err)
		cmd := shell("test \"$(cat input)\" = before; printf after > input")
		cmd.OutputPaths = []string{"input"}
		result := success(t, r.execute(t, cmd, &repb.Directory{Files: []*repb.FileNode{{Name: "input", Digest: d}}}))
		require.Len(t, result.OutputFiles, 1)
		require.Equal(t, []byte("after"), r.read(t, result.OutputFiles[0].Digest))
		require.Equal(t, []byte("before"), r.read(t, d))
	})
	t.Run("large-streaming-output", func(t *testing.T) {
		// 80 MiB exceeds both the harness gRPC message limit and typical batch CAS limits.
		cmd := shell("dd if=/dev/zero of=large bs=1048576 count=80 2>/dev/null")
		cmd.OutputPaths = []string{"large"}
		result := success(t, r.execute(t, cmd, nil))
		require.Len(t, result.OutputFiles, 1)
		data := r.read(t, result.OutputFiles[0].Digest)
		require.Len(t, data, 80<<20)
		require.True(t, bytes.Equal(data, make([]byte, 80<<20)))
	})
	t.Run("modern-overrides-legacy-selection", func(t *testing.T) {
		cmd := shell("printf selected > out; printf ignored > old")
		cmd.OutputPaths = []string{"out"}
		cmd.OutputFiles = []string{"old"}
		result := success(t, r.execute(t, cmd, nil))
		require.Len(t, result.OutputFiles, 1)
		require.Equal(t, "out", result.OutputFiles[0].Path)
	})
}
