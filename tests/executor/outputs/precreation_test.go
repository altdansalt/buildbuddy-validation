package outputs_test

import (
	"github.com/stretchr/testify/require"
	"testing"
)

// Legacy fields must not alter the workspace when output_paths supersedes them.
func TestOutputPathsIgnoreLegacyDirectoryCreation(t *testing.T) {
	r := start(t)
	cmd := shell("if test -e ignored; then printf unexpected > out; else printf expected > out; fi")
	cmd.OutputPaths = []string{"out"}
	cmd.OutputDirectories = []string{"ignored"}
	result := success(t, r.execute(t, cmd, nil))
	require.Len(t, result.OutputFiles, 1)
	require.Equal(t, "out", result.OutputFiles[0].Path)
	require.Empty(t, result.OutputDirectories)
	require.Equal(t, []byte("expected"), r.read(t, result.OutputFiles[0].Digest), "ignored output_directories must not change the execution filesystem")
}
