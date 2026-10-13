package projections_test

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

// Download a checksum-pinned released CLI, never compile BuildBuddy source.
func releasedCLI(t *testing.T, f *harness.Fixture) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	b, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	require.NoError(t, err)
	require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(b)))
	dir := t.TempDir()
	binary := filepath.Join(dir, "bb")
	require.NoError(t, os.WriteFile(binary, b, 0755))
	for _, args := range [][]string{{"init"}, {"config", "buildbuddy.api-key", f.APIKey}} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	return binary, dir
}
func runCLI(t *testing.T, binary, dir string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	// Only the disposable repository key is available to the subprocess.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TERM=dumb"}
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", b)
	return b
}
