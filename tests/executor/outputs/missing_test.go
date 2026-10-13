package outputs_test

import (
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestLegacyDirectoryIsPrecreated(t *testing.T) {
	r := start(t)
	// Compatibility with legacy Bazel rules intentionally precreates the directory.
	cmd := shell("test -d missing")
	cmd.OutputDirectories = []string{"missing"}
	result := success(t, r.execute(t, cmd, nil))
	require.Empty(t, result.OutputFiles)
	require.Len(t, result.OutputDirectories, 1)
	require.Equal(t, "missing", result.OutputDirectories[0].Path)
	tree := &repb.Tree{}
	require.NoError(t, proto.Unmarshal(r.read(t, result.OutputDirectories[0].TreeDigest), tree))
	require.NotNil(t, tree.Root)
	require.Empty(t, tree.Root.Files)
	require.Empty(t, tree.Root.Directories)
	require.Empty(t, tree.Root.Symlinks)
	require.Empty(t, tree.Children)
}
