package remoteasset_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__remote_asset_preflight" {
			w.Write([]byte("preflight"))
			return
		}
		handler.ServeHTTP(w, r)
	}), ReadHeaderTimeout: time.Second}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	origin := fmt.Sprintf("http://%s:%d", ip, listener.Addr().(*net.TCPAddr).Port)
	var f *harness.Fixture
	t.Cleanup(func() {
		if !t.Failed() || f == nil {
			return
		}
		files, _ := filepath.Glob(filepath.Join(f.OutputDir, "*-app.logs.txt"))
		for _, p := range files {
			b, _ := os.ReadFile(p)
			for _, line := range strings.Split(string(b), "\n") {
				if strings.Contains(line, "listening") || strings.Contains(line, "connection refused") {
					t.Log(line)
				}
			}
		}
		files, _ = filepath.Glob(filepath.Join(f.OutputDir, "*-app.inspect.txt"))
		for _, p := range files {
			b, _ := os.ReadFile(p)
			var v []struct {
				Config struct{ Cmd, Entrypoint []string }
			}
			if json.Unmarshal(b, &v) == nil {
				for _, item := range v {
					t.Logf("app entrypoint=%v command=%v", item.Config.Entrypoint, item.Config.Cmd)
				}
			}
		}
	})
	f = harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{
		harness.App: {"remote_asset": harness.Config{"allowed_private_ips": []string{ip.String() + "/32"}}},
	}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	fetch := asset.NewFetchClient(c.Conn)
	// Readiness can precede the app's self-RPC client reconnecting after its
	// initial connection attempt. Require the actual Fetch-to-CAS path to work.
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{origin + "/__remote_asset_preflight"}})
		if err == nil && resp.GetStatus().GetCode() == 0 {
			data, readErr := c.Read(ctx, "", resp.BlobDigest)
			require.NoError(t, readErr)
			require.Equal(t, "preflight", string(data))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Fetch-to-CAS preflight failed: response=%v error=%v", resp, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return c, fetch, origin, ctx
}
func sri(data string) *asset.Qualifier {
	h := sha256.Sum256([]byte(data))
	return &asset.Qualifier{Name: "checksum.sri", Value: "sha256-" + base64.StdEncoding.EncodeToString(h[:])}
}
func harnessDigest(data string) string { return harness.Digest([]byte(data)).Hash }
