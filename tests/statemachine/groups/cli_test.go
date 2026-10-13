package groups_test

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

//go:embed cli_pty.py
var cliPTY []byte

// Fetch a released CLI, not a source build. Both URL and checksum are fixed.
func releasedCLI(t *testing.T) (string, string, []string) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	b, err := io.ReadAll(io.LimitReader(res.Body, 100<<20))
	require.NoError(t, err)
	require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(b)))
	path := filepath.Join(dir, "bb")
	require.NoError(t, os.WriteFile(path, b, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "picker.py"), cliPTY, 0600))
	// An allowlist prevents inherited dispatch credentials from reaching the CLI.
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "TERM=xterm-256color", "LANG=C.UTF-8"}
	cmd := exec.CommandContext(ctx, "git", "init", dir)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return path, dir, env
}

func checkCLI(t *testing.T, binary, dir string, env []string, target, key string, expected int, names []string, selected int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--local", "buildbuddy.api-key", key)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	cmd = exec.CommandContext(ctx, binary, "login", "--check", "--target=grpc://"+target)
	cmd.Dir = dir
	cmd.Env = env
	out, err = cmd.CombinedOutput()
	code := 0
	if err != nil {
		e, ok := err.(*exec.ExitError)
		require.True(t, ok, "CLI failed: %v", err)
		code = e.ExitCode()
	}
	require.Equal(t, expected, code, "bb login --check: %s", out)
	if names == nil {
		return
	}
	b, err := json.Marshal(names)
	require.NoError(t, err)
	cmd = exec.CommandContext(ctx, "python3", filepath.Join(dir, "picker.py"), binary, "grpc://"+target, string(b), fmt.Sprint(selected))
	cmd.Dir = dir
	cmd.Env = env
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "bb ui: %s", out)
}
