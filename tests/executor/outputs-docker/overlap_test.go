package outputs_test

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOverlappingDirectoryOutputs(t *testing.T) {
	r := start(t)
	cmd := shell("mkdir -p tree; printf inner > tree/file")
	cmd.OutputPaths = []string{"tree", "tree/file"}
	result := success(t, r.execute(t, cmd, nil))
	require.Len(t, result.OutputDirectories, 1)
	require.Len(t, result.OutputFiles, 1, "explicitly declared file must be returned even beneath an output directory")
	require.Equal(t, "tree/file", result.OutputFiles[0].Path)
	require.Equal(t, []byte("inner"), r.read(t, result.OutputFiles[0].Digest))
}
