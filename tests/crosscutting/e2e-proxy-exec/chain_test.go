package chain_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

// A real Bazel client runs inside this bb remote test runner. This is a client
// workload, not a local build of BuildBuddy or of the validation suite.
func bazelClient(t *testing.T, ctx context.Context) string {
	t.Helper()
	const base = "https://releases.bazel.build/8.4.2/release/bazel-8.4.2-linux-x86_64"
	fetch := func(url string) []byte {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		require.NoError(t, err)
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer r.Body.Close()
		require.Equal(t, http.StatusOK, r.StatusCode)
		b, err := io.ReadAll(io.LimitReader(r.Body, 100<<20))
		require.NoError(t, err)
		return b
	}
	b := fetch(base)
	sum := sha256.Sum256(b)
	require.Equal(t, strings.Fields(string(fetch(base + ".sha256")))[0], hex.EncodeToString(sum[:]))
	path := filepath.Join(t.TempDir(), "bazel")
	require.NoError(t, os.WriteFile(path, b, 0755))
	return path
}

func TestBazelProxyExecutionChain(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.CacheProxy, harness.Executor},
		Config: map[harness.Component]harness.Config{
			harness.Executor: {"executor": harness.Config{"cache_target": "grpc://cacheproxy:1985"}},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	bazel := bazelClient(t, ctx)
	workspace := t.TempDir()
	install := filepath.Join(t.TempDir(), "install")
	write := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(workspace, name), []byte(content), 0644))
	}
	write("MODULE.bazel", "module(name = \"chain\")\n")
	write("WORKSPACE", "workspace(name = \"chain\")\n")
	write("BUILD.bazel", `genrule(
 name = "copy", srcs = ["input.bin"], outs = ["result.bin", "empty.bin"],
 cmd = "cat $(location input.bin) > $(location result.bin); : > $(location empty.bin); echo chain-stdout; echo chain-stderr >&2",
)
genrule(name = "fail", outs = ["failed.txt"], cmd = "echo expected-failure >&2; exit 19")
`)
	// Exceeds typical inline/batch thresholds and contains every byte value.
	alphabet := make([]byte, 256)
	for i := range alphabet {
		alphabet[i] = byte(i)
	}
	payload := bytes.Repeat(alphabet, 5000)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "input.bin"), payload, 0644))
	run := func(t *testing.T, target string, want []byte, cached bool, fails bool) {
		t.Helper()
		out := t.TempDir()
		logPath := filepath.Join(out, "execution.json")
		args := []string{"--batch", "--bazelrc=/dev/null", "--output_base=" + out, "--install_base=" + install,
			"build", target, "--enable_bzlmod=false", "--enable_workspace=true", "--incompatible_autoload_externally=", "--spawn_strategy=remote", "--genrule_strategy=remote",
			"--remote_cache=grpc://" + f.Endpoint(harness.CacheProxy).GRPC, "--remote_executor=grpc://" + f.Endpoint(harness.App).GRPC,
			"--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--remote_instance_name=chain", "--remote_timeout=90s",
			"--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64",
			"--remote_default_exec_properties=workload-isolation-type=none", "--remote_download_outputs=all",
			"--remote_local_fallback=false", "--execution_log_json_file=" + logPath, "--jobs=4", "--color=no", "--curses=no"}
		cmd := exec.CommandContext(ctx, bazel, args...)
		cmd.Dir = workspace
		// Avoid inherited credentials and user Bazel configuration in the client workload.
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + workspace, "TMPDIR=" + os.TempDir()}
		output, err := cmd.CombinedOutput()
		// Do not print the API key-bearing command line.
		t.Logf("Bazel %s:\n%s", target, output)
		if fails {
			require.Error(t, err)
			require.Contains(t, string(output), "expected-failure")
		} else {
			require.NoError(t, err, "%s", output)
			require.Contains(t, string(output), "chain-stdout")
			require.Contains(t, string(output), "chain-stderr")
			actual, err := os.ReadFile(filepath.Join(out, "execroot", "chain", "bazel-out", "k8-fastbuild", "bin", "result.bin"))
			require.NoError(t, err)
			require.Equal(t, want, actual)
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				blob, readErr := f.Clients(component).Read(ctx, "chain", harness.Digest(want))
				require.NoError(t, readErr, "output must be visible through %s", component)
				require.Equal(t, want, blob)
			}
			empty, err := os.ReadFile(filepath.Join(out, "execroot", "chain", "bazel-out", "k8-fastbuild", "bin", "empty.bin"))
			require.NoError(t, err)
			require.Empty(t, empty)
		}
		logData, err := os.ReadFile(logPath)
		require.NoError(t, err)
		decoder := json.NewDecoder(bytes.NewReader(logData))
		found := false
		for decoder.More() {
			var entry struct {
				Mnemonic       string `json:"mnemonic"`
				Runner         string `json:"runner"`
				CacheHit       bool   `json:"cacheHit"`
				RemoteCacheHit bool   `json:"remoteCacheHit"`
				ExitCode       int    `json:"exitCode"`
			}
			require.NoError(t, decoder.Decode(&entry))
			if entry.Mnemonic != "Genrule" {
				continue
			}
			found = true
			t.Logf("execution: %+v", entry)
			require.Equal(t, cached, entry.CacheHit || entry.RemoteCacheHit, "fresh output base must distinguish execution from remote AC hit")
			require.Contains(t, entry.Runner, "remote")
			if fails {
				require.Equal(t, 19, entry.ExitCode)
			} else {
				require.Zero(t, entry.ExitCode)
			}
		}
		require.True(t, found, "missing Genrule execution record: %s", logData)
	}
	for i, name := range []string{"cold", "warm", "changed", "reverted"} {
		t.Run(name, func(t *testing.T) {
			data := payload
			if i == 2 {
				data = append([]byte("changed\x00"), payload...)
			}
			require.NoError(t, os.WriteFile(filepath.Join(workspace, "input.bin"), data, 0644))
			run(t, "//:copy", data, i == 1 || i == 3, false)
		})
	}
	for i := 0; i < 2; i++ {
		t.Run(fmt.Sprintf("failure-%d", i), func(t *testing.T) { run(t, "//:fail", nil, false, true) })
	}
}
