package outputs_test

import (
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestIgnoredLegacySymlinkDoesNotEscapeDirectoryTree(t *testing.T) {
	r := start(t)
	cmd := shell("mkdir tree; printf data > target; ln -s ../target tree/link")
	cmd.OutputPaths = []string{"tree"}
	cmd.OutputFiles = []string{"tree/link"}
	result := success(t, r.execute(t, cmd, nil))
	require.Len(t, result.OutputDirectories, 1)
	require.Equal(t, "tree", result.OutputDirectories[0].Path)
	tree := &repb.Tree{}
	require.NoError(t, proto.Unmarshal(r.read(t, result.OutputDirectories[0].TreeDigest), tree))
	require.Len(t, tree.Root.Symlinks, 1)
	require.Equal(t, "link", tree.Root.Symlinks[0].Name)
	require.Equal(t, "../target", tree.Root.Symlinks[0].Target)
	require.Empty(t, result.OutputFiles)
	require.Empty(t, result.OutputSymlinks)
	require.Empty(t, result.OutputFileSymlinks, "output_files is ignored when output_paths is set; tree/link belongs only in the directory Tree")
	require.Empty(t, result.OutputDirectorySymlinks)
}
