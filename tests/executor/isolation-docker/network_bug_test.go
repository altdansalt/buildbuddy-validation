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
			r := execute(t, f, "mirror.gcr.io/library/alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c", `ls /sys/class/net; cat /proc/net/route`, "", props)
			success(t, r)
			out := stdout(t, f, r)
			t.Logf("network=off container interfaces and routes:\n%s", out)
			require.NotContains(t, out, "eth0", "network=off must disable the configured bridge")
		})
	}
}
