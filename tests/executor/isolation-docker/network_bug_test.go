package isolation_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// GetEffectiveDockerNetwork translates network=off to "none", but the Docker
// hostConfig switch handles only "off". The configured bridge remains enabled.
func TestNetworkOff(t *testing.T) {
	f := fixture(t)
	for _, legacy := range []string{"", "host"} {
		t.Run("dockerNetwork="+legacy, func(t *testing.T) {
			props := map[string]string{"network": "off"}
			if legacy != "" {
				props["dockerNetwork"] = legacy
			}
			r := execute(t, f, "alpine:3.21.3", `ls /sys/class/net; cat /proc/net/route`, "", props)
			success(t, r)
			out := stdout(t, f, r)
			t.Logf("network=off container interfaces and routes:\n%s", out)
			require.NotContains(t, out, "eth0", "network=off must disable the configured bridge")
		})
	}
}
