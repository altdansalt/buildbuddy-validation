package outputs_test

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOutputPathsOverrideLegacySymlinks(t *testing.T) {
	r := start(t)
	cmd := shell("printf data > target; ln -s target link")
	cmd.OutputPaths = []string{"link"}
	cmd.OutputFiles = []string{"ignored"}
	result := success(t, r.execute(t, cmd, nil))
	require.Len(t, result.OutputSymlinks, 1, "output_paths supersedes output_files; requested symlink must be returned")
	require.Equal(t, "link", result.OutputSymlinks[0].Path)
	require.Equal(t, "target", result.OutputSymlinks[0].Target)
}
