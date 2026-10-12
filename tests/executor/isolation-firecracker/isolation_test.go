package isolation_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

func TestBareStateBetweenActions(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Logf("KVM unavailable on test runner (%v); exercising none isolation", err)
	} else {
		t.Log("KVM device exists; Firecracker additionally requires executor device passthrough and provider configuration")
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	// Non-recycled actions have separate workspaces and managed temporary dirs.
	first := execute(t, c, `set -eux; test -n "$TMPDIR"; printf '%s\n%s\n' "$PWD" "$TMPDIR"; mkdir -p stale/sub; echo stale > stale/sub/file; ln -s input stale-link; rm input; echo changed > input; echo secret > "$TMPDIR/marker"`, false, []*repb.Command_EnvironmentVariable{{Name: "ACTION_SECRET", Value: "first-only"}})
	paths := strings.Split(strings.TrimSpace(first), "\n")
	require.Len(t, paths, 2)
	// Cleanup is asynchronous after completion; poll only fixture-local paths.
	script := fmt.Sprintf(`set -eux; test -z "${ACTION_SECRET-}"; test "$(cat input)" = original; test ! -e stale; test ! -L stale-link; test "$PWD" != '%s'; test "$TMPDIR" != '%s'; i=0; while test -e '%s' || test -e '%s'; do i=$((i+1)); test "$i" -lt 100 || { echo cleanup-did-not-complete >&2; exit 1; }; sleep 0.1; done; echo clean`, paths[0], paths[1], paths[0], paths[1])
	require.Equal(t, "clean\n", execute(t, c, script, false, nil))
}

func TestRecycledWorkspaceIsClean(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"runner_pool": harness.Config{"max_runner_count": 1}}}}})
	c := f.Clients(harness.App)
	seen := make(map[string]bool)
	reused := false
	for i := 0; i < 8; i++ {
		// Each action dirties both declared input and undeclared nested/symlink state.
		// Default preserve-workspace=false must remove these before pool reuse.
		out := execute(t, c, fmt.Sprintf(`set -eux; test "$(cat input)" = original; test ! -e stale; test ! -L stale-link; test -z "${PREVIOUS_ACTION-}"; printf '%%s\n' "$PWD"; mkdir -p stale/nested; echo %d > stale/nested/file; ln -s input stale-link; rm input; echo mutated > input; export PREVIOUS_ACTION=yes`, i), true, nil)
		path := strings.TrimSpace(out)
		if seen[path] {
			reused = true
		}
		seen[path] = true
		// Allow the pool's asynchronous post-completion recycling to finish.
		time.Sleep(250 * time.Millisecond)
	}
	require.True(t, reused, "no workspace reuse observed; cleanup assertions did not exercise recycling")
}
