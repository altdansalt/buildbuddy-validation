package outputs_test

import (
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestOverlappingDirectories(t *testing.T) {
	r := start(t)
	cmd := shell("mkdir -p tree/nested; printf inner > tree/nested/file")
	cmd.OutputPaths = []string{"tree", "tree/nested"}
	result := success(t, r.execute(t, cmd, nil))
	require.Len(t, result.OutputDirectories, 2)
	trees := map[string]*repb.Tree{}
	for _, out := range result.OutputDirectories {
		tree := &repb.Tree{}
		require.NoError(t, proto.Unmarshal(r.read(t, out.TreeDigest), tree))
		trees[out.Path] = tree
	}
	require.NotNil(t, trees["tree"])
	require.NotNil(t, trees["tree/nested"])
	require.Len(t, trees["tree"].Children, 1)
	require.True(t, proto.Equal(trees["tree"].Children[0], trees["tree/nested"].Root))
	require.Len(t, trees["tree/nested"].Root.Files, 1)
	require.Equal(t, []byte("inner"), r.read(t, trees["tree/nested"].Root.Files[0].Digest))
}
