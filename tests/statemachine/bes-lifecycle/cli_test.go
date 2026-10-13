package bes_lifecycle_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

// Download a released CLI binary, pinned by digest. The subprocess receives only
// fixture credentials; it inherits no dispatch credentials or user configuration.
var cliPaths = map[*testing.T]string{}

func cliOutput(t *testing.T, ctx context.Context, f *harness.Fixture, id string) ([]byte, error) {
	t.Helper()
	path := cliPaths[t]
	if path == "" {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
		require.NoError(t, err)
		resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
		data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
		require.NoError(t, err)
		require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(data)))
		dir := t.TempDir()
		path = filepath.Join(dir, "bb")
		require.NoError(t, os.WriteFile(path, data, 0755))
		cliPaths[t] = path
	}
	dir := filepath.Dir(path)
	cmd := exec.CommandContext(ctx, path, "view", id, "--target=grpc://"+f.Endpoint(harness.App).GRPC)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "BUILDBUDDY_API_KEY=" + f.APIKey}
	return cmd.CombinedOutput()
}

func checkCLI(t *testing.T, ctx context.Context, f *harness.Fixture, id, want string) {
	t.Helper()
	out, err := cliOutput(t, ctx, f, id)
	require.NoError(t, err)
	if want == "" {
		require.Empty(t, string(out))
	} else {
		require.Equal(t, "===== BUILD LOGS =====\n"+want, string(out))
	}
}
