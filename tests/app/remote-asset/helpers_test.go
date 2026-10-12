package remoteasset_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	asset "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	"github.com/stretchr/testify/require"
)

// The origin listens on this runner, never a shared service. Container bridge
// traffic can reach the runner's own interface without exposing Docker internals.
func setup(t *testing.T, handler http.Handler) (*harness.Clients, asset.FetchClient, string, context.Context) {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	require.NoError(t, err)
	var ip net.IP
	for _, address := range addresses {
		if n, ok := address.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			ip = n.IP
			break
		}
	}
	require.NotNil(t, ip, "runner needs a non-loopback IPv4 interface")
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	origin := fmt.Sprintf("http://%s:%d", ip, listener.Addr().(*net.TCPAddr).Port)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{
		harness.App: {"remote_asset": harness.Config{"allowed_private_ips": []string{ip.String() + "/32"}}},
	}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c, asset.NewFetchClient(c.Conn), origin, ctx
}
func sri(data string) *asset.Qualifier {
	h := sha256.Sum256([]byte(data))
	return &asset.Qualifier{Name: "checksum.sri", Value: "sha256-" + base64.StdEncoding.EncodeToString(h[:])}
}
func harnessDigest(data string) string { return harness.Digest([]byte(data)).Hash }
