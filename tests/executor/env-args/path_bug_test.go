package envargs_test

import (
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"testing"
)

// REAPI Command.arguments says an explicitly supplied PATH SHOULD be respected.
func TestCommandPATH(t *testing.T) {
	c := fixture(t)
	root := scriptTree(t, c)
	// Control proves the input tree, executable bit, shebang, and cwd are valid.
	require.Equal(t, "input-tool:control\n", stdout(t, c, execute(t, c, root, &repb.Command{Arguments: []string{"./envargs-tool", "control"}, WorkingDirectory: "tools"})))
	for _, path := range []string{".", "missing:.", "", ":/nonexistent"} {
		t.Run("PATH="+path, func(t *testing.T) {
			require.Equal(t, "input-tool:path\n", stdout(t, c, execute(t, c, root, &repb.Command{Arguments: []string{"envargs-tool", "path"}, WorkingDirectory: "tools", EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "PATH", Value: path}}})))
		})
	}
}
