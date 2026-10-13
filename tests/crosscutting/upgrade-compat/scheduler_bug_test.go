package upgradecompat_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"testing"
)

func TestRunningWorkUpgrade(t *testing.T) {
	schedulerRecovery(t, true, oldApp, harness.PinnedImages[harness.App])
}
func TestRunningWorkCurrentRestart(t *testing.T) {
	schedulerRecovery(t, true, harness.PinnedImages[harness.App], harness.PinnedImages[harness.App])
}
