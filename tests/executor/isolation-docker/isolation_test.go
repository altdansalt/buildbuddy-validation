package isolation_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

func TestDockerIsolation(t *testing.T) {
	f := fixture(t)
	t.Run("tag_workdir_env_mount", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `cat input.txt > output.txt; printf '%s\n' "$CONFORMANCE_VALUE"; test -f /etc/alpine-release; test ! -S /var/run/docker.sock`, "sub", nil)
		success(t, r)
		require.Equal(t, "spaces = and unicode λ\n", stdout(t, f, r))
		require.Len(t, r.Result.OutputFiles, 1)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		b, err := f.Clients(harness.App).Read(ctx, "docker-isolation", r.Result.OutputFiles[0].Digest)
		require.NoError(t, err)
		require.Equal(t, "mounted input\n", string(b))
	})
	t.Run("network_on", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `test -d /sys/class/net/eth0; cat /proc/net/route`, "", nil)
		success(t, r)
		require.Contains(t, stdout(t, f, r), "eth0")
	})
	t.Run("readonly_mount", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `cat /conformance-extra/marker; if echo overwrite > /conformance-extra/marker 2>/dev/null; then exit 1; fi`, "", nil)
		success(t, r)
		require.Equal(t, "read-only mount\n", stdout(t, f, r))
	})
	t.Run("numeric_user", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", "id -u; id -g; cat sub/input.txt", "", map[string]string{"dockerUser": "12345:23456"})
		success(t, r)
		require.Equal(t, "12345\n23456\nmounted input\n", stdout(t, f, r))
	})
	for _, property := range []string{"dockerNetwork"} {
		t.Run(property+"_off", func(t *testing.T) {
			r := execute(t, f, "alpine:3.21.3", `ls /sys/class/net; cat /proc/net/route`, "", map[string]string{property: "off"})
			success(t, r)
			out := stdout(t, f, r)
			require.True(t, strings.HasPrefix(out, "lo\n"), out)
			require.NotContains(t, out, "eth0")
		})
	}
	t.Run("fresh_container", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `echo dirty > /tmp/action-marker; echo dirty > sub/unlisted; echo first`, "", nil)
		success(t, r)
		r = execute(t, f, "alpine:3.21.3", `test ! -e /tmp/action-marker && test ! -e sub/unlisted && echo clean`, "", nil)
		success(t, r)
		require.Equal(t, "clean\n", stdout(t, f, r))
	})
	t.Run("nonzero_exit", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", `echo expected >&2; exit 37`, "", nil)
		require.Zero(t, r.GetStatus().GetCode())
		require.EqualValues(t, 37, r.GetResult().GetExitCode())
	})
	t.Run("digest", func(t *testing.T) {
		b, err := exec.Command("docker", "image", "inspect", "alpine:3.21.3", "--format", "{{index .RepoDigests 0}}").CombinedOutput()
		require.NoError(t, err, string(b))
		ref := strings.TrimSpace(string(b))
		require.Contains(t, ref, "@sha256:")
		r := execute(t, f, ref, "cat /etc/alpine-release", "", nil)
		success(t, r)
		require.Equal(t, "3.21.3\n", stdout(t, f, r))
	})
	t.Run("invalid_network", func(t *testing.T) {
		r := execute(t, f, "alpine:3.21.3", "echo must-not-run", "", map[string]string{"network": "invalid"})
		require.EqualValues(t, 3, r.GetStatus().GetCode())
		require.Empty(t, stdout(t, f, r))
	})
	t.Run("private_registry_denied", func(t *testing.T) {
		var requests atomic.Int64
		registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			requests.Add(1)
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture-private-registry"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)
		}))
		defer registry.Close()
		// Docker treats loopback registries as insecure, so no shared daemon config
		// or external authentication provider is needed for this denial oracle.
		image := strings.TrimPrefix(registry.URL, "http://") + "/private:latest"
		r := execute(t, f, image, "echo must-not-run", "", nil)
		require.Positive(t, requests.Load(), "pull must reach our local registry")
		require.NotZero(t, r.GetStatus().GetCode())
		require.Empty(t, stdout(t, f, r))
		require.Contains(t, strings.ToLower(r.GetStatus().GetMessage()), "auth")
	})
	t.Run("missing_tag", func(t *testing.T) {
		r := execute(t, f, "alpine:bbval-nonexistent-tag-20261012", "echo must-not-run", "", nil)
		require.NotZero(t, r.GetStatus().GetCode())
		require.Empty(t, stdout(t, f, r))
		require.Contains(t, strings.ToLower(r.GetStatus().GetMessage()), "pull")
	})
	// No action containers should remain after completed non-recycled actions.
	deadline := time.Now().Add(15 * time.Second)
	for {
		b, err := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}").CombinedOutput()
		require.NoError(t, err)
		names := strings.Fields(string(b))
		if len(names) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("action containers remain: %s", b)
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, err := exec.Command("docker", "logs", f.Endpoint(harness.Executor).Container).CombinedOutput()
	require.NoError(t, err)
	require.NotContains(t, string(logs), "panic:")
	require.NotContains(t, string(logs), "fatal error:")
}
