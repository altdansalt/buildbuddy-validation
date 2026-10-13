package outputs_test

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMissingLegacyDirectoryIsOmitted(t *testing.T) {
	r := start(t)
	cmd := shell(":")
	cmd.OutputDirectories = []string{"missing"}
	result := success(t, r.execute(t, cmd, nil))
	require.Empty(t, result.OutputDirectories, "an output the command never created must be omitted")
}
