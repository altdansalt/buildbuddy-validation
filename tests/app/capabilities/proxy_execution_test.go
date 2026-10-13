package capabilities_test

import (
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

func TestProxyExecutionAdvertisement(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	// Control: the upstream app serves Execution under the same configuration.
	checkExecutionService(t, f.Clients(harness.App), true)
	checkExecutionService(t, f.Clients(harness.CacheProxy), false)
}
