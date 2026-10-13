package githubstate

import "testing"

// github.proto documents read_write_app_installed as installed AND linked to
// the org. Removing the installation must clear that derived flag, while
// preserving the repository row as documented by UnlinkAppInstallationRequest.
func TestUnlinkInstallationClearsRepositoryFlag(t *testing.T) {
	runStateMachine(t, true)
}
