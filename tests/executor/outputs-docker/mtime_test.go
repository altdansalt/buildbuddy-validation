package outputs_test

import (
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestRequestedMtime(t *testing.T) {
	r := start(t)
	cmd := shell("mkdir tree; printf content > file; printf nested > tree/file; touch -t 200001010000 file tree/file tree")
	cmd.OutputPaths = []string{"file", "tree"}
	cmd.OutputNodeProperties = []string{"mtime"}
	response := r.execute(t, cmd, nil)
	// A server may reject properties it does not support, but must not silently
	// accept the request and omit the requested metadata.
	if response.GetStatus().GetCode() == 3 {
		t.Log("mtime explicitly unsupported")
		return
	}
	result := success(t, response)
	require.Len(t, result.OutputFiles, 1)
	require.NotNil(t, result.OutputFiles[0].GetNodeProperties().GetMtime(), "requested output file mtime omitted")
	require.EqualValues(t, 946684800, result.OutputFiles[0].NodeProperties.Mtime.Seconds)
	require.Zero(t, result.OutputFiles[0].NodeProperties.Mtime.Nanos)
	require.Len(t, result.OutputDirectories, 1)
	tree := &repb.Tree{}
	require.NoError(t, proto.Unmarshal(r.read(t, result.OutputDirectories[0].TreeDigest), tree))
	require.NotNil(t, tree.Root.GetNodeProperties().GetMtime(), "requested directory mtime omitted")
	require.EqualValues(t, 946684800, tree.Root.NodeProperties.Mtime.Seconds)
	require.Len(t, tree.Root.Files, 1)
	require.NotNil(t, tree.Root.Files[0].GetNodeProperties().GetMtime(), "requested nested file mtime omitted")
	require.EqualValues(t, 946684800, tree.Root.Files[0].NodeProperties.Mtime.Seconds)
}
