package executionstate_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func downloadCLI(t *testing.T, ctx context.Context, f *harness.Fixture, d *repb.Digest, want []byte) {
	t.Helper()
	// Released executable, pinned by content; never build BuildBuddy source.
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, 200, response.StatusCode)
	binary, err := io.ReadAll(io.LimitReader(response.Body, 100<<20))
	require.NoError(t, err)
	require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(binary)))
	dir := t.TempDir()
	path := filepath.Join(dir, "bb")
	require.NoError(t, os.WriteFile(path, binary, 0700))
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		cmd := exec.CommandContext(ctx, path, "download", fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey)
		cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
		cmd.Dir = dir
		output, err := cmd.Output()
		if failure, ok := err.(*exec.ExitError); ok {
			t.Logf("bb download stderr: %s", failure.Stderr)
		}
		require.NoError(t, err)
		require.Equal(t, want, output)
	}
}
