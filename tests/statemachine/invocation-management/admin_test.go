package invocation_management_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

// Set the disposable group's server-admin role before ingesting any test state.
// Restart preserves fixture storage. No component requests leave this fixture.
func configureOwnerRead(t *testing.T, f *harness.Fixture, olap ...bool) {
	t.Helper()
	hosts := []string{""}
	sockets, e := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	require.NoError(t, e)
	for _, socket := range sockets {
		hosts = append(hosts, "unix://"+socket)
	}
	var dockerHost string
	found := false
	for _, host := range hosts {
		args := []string{}
		if host != "" {
			args = append(args, "--host", host)
		}
		args = append(args, "inspect", f.Endpoint(harness.App).Container)
		if exec.Command("docker", args...).Run() == nil {
			dockerHost = host
			found = true
			break
		}
	}
	require.True(t, found, "fixture Docker daemon must be reachable")
	docker := func(args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if dockerHost != "" {
			args = append([]string{"--host", dockerHost}, args...)
		}
		out, e := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		require.NoError(t, e, string(out))
	}
	if len(olap) > 0 && olap[0] {
		name := f.ID + "-clickhouse"
		docker("run", "-d", "--name", name, "--network", f.Network, "--network-alias", "clickhouse", "--env", "CLICKHOUSE_SKIP_USER_SETUP=1", "mirror.gcr.io/clickhouse/clickhouse-server@sha256:b627d7a9bc0e0c1bac26cdbe9d2fc6316faa29c5d8a174f28f5abd57d0fa6ba2")
		t.Cleanup(func() { docker("rm", "-f", name) })
		deadline := time.Now().Add(time.Minute)
		for {
			args := []string{"exec", name, "clickhouse-client", "--query", "SELECT 1"}
			if dockerHost != "" {
				args = append([]string{"--host", dockerHost}, args...)
			}
			if exec.Command("docker", args...).Run() == nil {
				break
			}
			require.True(t, time.Now().Before(deadline), "ClickHouse readiness")
			time.Sleep(200 * time.Millisecond)
		}
	}
	path := filepath.Join(f.OutputDir, "app.yaml")
	data, e := os.ReadFile(path)
	require.NoError(t, e)
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		if line == "auth:" {
			indent := lines[i+1][:len(lines[i+1])-len(strings.TrimLeft(lines[i+1], " "))]
			lines[i] += "\n" + indent + "admin_group_id: " + f.GroupID
			changed = true
			break
		}
	}
	require.True(t, changed)
	if len(olap) > 0 && olap[0] {
		lines = append(lines, "olap_database:", "  data_source: clickhouse://default:@clickhouse:9000/default")
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600))
	docker("cp", path, f.Endpoint(harness.App).Container+":/config.yaml")
	docker("restart", f.Endpoint(harness.App).Container)
	port := func(p string) string {
		args := []string{"port", f.Endpoint(harness.App).Container, p}
		if dockerHost != "" {
			args = append([]string{"--host", dockerHost}, args...)
		}
		out, err := exec.Command("docker", args...).CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	f.Endpoint(harness.App).HTTP = "http://" + port("8080/tcp")
	f.Endpoint(harness.App).GRPC = port("1985/tcp")
	deadline := time.Now().Add(time.Minute)
	ready := false
	for time.Now().Before(deadline) {
		resp, e := (&http.Client{Timeout: time.Second}).Get(f.Endpoint(harness.App).HTTP + "/readyz?server-type=app")
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	require.True(t, ready)
	f.Web = harness.LoginSelfAuth(t, f.Endpoint(harness.App))
}
