package membership

import "testing"

// Immediate cross-component consistency currently fails with the proxy's
// default JWT TTL. The untagged suite separately requires eventual convergence.
func TestImmediateProxyMembershipRevocation(t *testing.T) {
	runProxyMembershipRevocation(t, true)
}
