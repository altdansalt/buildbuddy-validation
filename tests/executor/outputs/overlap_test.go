package outputs_test

import (
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestOverlappingDirectoryOutputs(t *testing.T) {
	r := start(t)
	cmd := shell("mkdir -p tree/nested; printf outer > tree/file; printf inner > tree/nested/file")
	cmd.OutputPaths = []string{"tree", "tree/nested", "tree/nested/file"}
	result := success(t, r.execute(t, cmd, nil))
	require.Len(t, result.OutputDirectories, 2)
	require.Len(t, result.OutputFiles, 1)
	require.Equal(t, "tree/nested/file", result.OutputFiles[0].Path)
	require.Equal(t, []byte("inner"), r.read(t, result.OutputFiles[0].Digest))
	trees := map[string]*repb.Tree{}
	for _, out := range result.OutputDirectories {
		tree := &repb.Tree{}
		require.NoError(t, proto.Unmarshal(r.read(t, out.TreeDigest), tree))
		trees[out.Path] = tree
		for _, dir := range append([]*repb.Directory{tree.Root}, tree.Children...) {
			for _, file := range dir.Files {
				r.read(t, file.Digest)
			}
		}
	}
	require.NotNil(t, trees["tree"])
	require.NotNil(t, trees["tree/nested"])
	require.Len(t, trees["tree"].Root.Directories, 1)
	require.Len(t, trees["tree/nested"].Root.Files, 1)
	require.True(t, proto.Equal(result.OutputFiles[0].Digest, trees["tree/nested"].Root.Files[0].Digest))
	require.Len(t, trees["tree"].Children, 1)
	require.True(t, proto.Equal(trees["tree"].Children[0], trees["tree/nested"].Root))
}
