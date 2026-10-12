package envargs_test

import (
	"fmt"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestArgumentsAndEnvironment(t *testing.T) {
	c := fixture(t)
	root := upload(t, c, &repb.Directory{})
	t.Run("argument-order-empty-and-literals", func(t *testing.T) {
		args := []string{"", "a b", "$HOME", "a=b", "quote'\"", "line\nbreak", "same", "same", "雪"}
		cmd := &repb.Command{Arguments: append([]string{"/bin/sh", "-c", `printf '%s\000' "$@"`, "argv0"}, args...)}
		require.Equal(t, strings.Join(args, "\x00")+"\x00", stdout(t, c, execute(t, c, root, cmd)))
	})
	t.Run("sorted-env-empty-equals-unicode", func(t *testing.T) {
		cmd := &repb.Command{Arguments: []string{"/bin/sh", "-c", `printf '<%s><%s><%s><%s>' "$A" "$EMPTY" "$UNICODE" "$Z"`}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "A", Value: "a=b=c"}, {Name: "EMPTY", Value: ""}, {Name: "UNICODE", Value: "雪"}, {Name: "Z", Value: "last"}}}
		require.Equal(t, "<a=b=c><><雪><last>", stdout(t, c, execute(t, c, root, cmd)))
	})
	// REAPI does not define duplicate-name precedence. Pin the released Go
	// executor's last-entry-wins behavior as regression coverage, not conformance.
	t.Run("duplicate-env-last-wins", func(t *testing.T) {
		cmd := &repb.Command{Arguments: []string{"/bin/sh", "-c", `printf '%s' "$A"`}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "A", Value: "first"}, {Name: "A", Value: "last"}}}
		require.Equal(t, "last", stdout(t, c, execute(t, c, root, cmd)))
	})
	t.Run("large-env-and-many-args", func(t *testing.T) {
		args := []string{"/bin/sh", "-c", `printf '%s:%s:%s' "$#" "${#BIG}" "$1000"`, "argv0"}
		for i := 0; i < 2000; i++ {
			args = append(args, fmt.Sprintf("arg-%04d", i))
		}
		cmd := &repb.Command{Arguments: args, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "BIG", Value: strings.Repeat("x", 64<<10)}}}
		// $1000 in POSIX sh means $1 followed by 000.
		require.Equal(t, "2000:65536:arg-0000000", stdout(t, c, execute(t, c, root, cmd)))
	})
}
func TestWorkingDirectory(t *testing.T) {
	c := fixture(t)
	root := scriptTree(t, c)
	t.Run("relative-executable-in-working-directory", func(t *testing.T) {
		require.Equal(t, "input-tool:relative\n", stdout(t, c, execute(t, c, root, &repb.Command{Arguments: []string{"./envargs-tool", "relative"}, WorkingDirectory: "tools"})))
	})
	for _, wd := range []string{"missing", "../", "tools/../../", "/tmp", "tools/envargs-tool"} {
		t.Run(wd, func(t *testing.T) {
			r := execute(t, c, root, &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf should-not-run"}, WorkingDirectory: wd})
			require.EqualValues(t, 9, r.GetStatus().GetCode(), "invalid working directory must be rejected before execution")
		})
	}
	// A rejected action must not poison the executor for subsequent requests.
	require.Equal(t, "healthy", stdout(t, c, execute(t, c, root, &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf healthy"}})))
}
