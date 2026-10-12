package outputs_test

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLegacyOutputTypeMismatch(t *testing.T) {
	r := start(t)
	for _, tc := range []struct {
		name, script string
		directory    bool
	}{
		{"directory-as-file", "mkdir wrong", false},
		{"file-as-directory", "rm -rf wrong; printf file > wrong", true},
		{"directory-symlink-as-file", "mkdir target; ln -s target wrong", false},
		{"file-symlink-as-directory", "rm -rf wrong; printf file > target; ln -s target wrong", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := shell(tc.script)
			if tc.directory {
				cmd.OutputDirectories = []string{"wrong"}
			} else {
				cmd.OutputFiles = []string{"wrong"}
			}
			response := r.execute(t, cmd, nil)
			require.EqualValues(t, 9, response.GetStatus().GetCode(), "REAPI requires FAILED_PRECONDITION for legacy output type mismatch; result=%v", response.GetResult())
		})
	}
}
