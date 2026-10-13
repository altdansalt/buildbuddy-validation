package bazelflags

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

var seed = flag.Int64("flags_seed", 83142, "Replay seed")
var iterations = flag.Int("flags_iterations", 4, "Randomized flag combinations (1..24)")

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// Download a fixed standalone Bazel, independently of the outer runner's launcher.
func bazel(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://releases.bazel.build/8.4.2/release/bazel-8.4.2-linux-x86_64", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("Bazel download: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != "4dc8e99dfa802e252dac176d08201fd15c542ae78c448c8a89974b6f387c282c" {
		t.Fatal("Bazel checksum mismatch")
	}
	path := filepath.Join(t.TempDir(), "bazel")
	write(t, path, b)
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func build(t *testing.T, bin, ws string, flags []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	args := []string{"--batch", "--host_jvm_args=-Xmx384m", "--output_user_root=" + t.TempDir(), "--ignore_all_rc_files", "build", "//:final", "--enable_bzlmod=false", "--enable_workspace=true", "--incompatible_autoload_externally=", "--jobs=2", "--spawn_strategy=local", "--color=no", "--curses=no"}
	args = append(args, flags...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = ws
	// No worker credentials or external rc configuration enters the nested build.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "TMPDIR=" + os.TempDir()}
	out, err := cmd.CombinedOutput()
	safe := strings.ReplaceAll(string(out), extractKey(flags), "<fixture-key>")
	if err != nil {
		t.Fatalf("seed=%d flags=%v: %v\n%s", *seed, redact(flags), err, safe)
	}
	return safe
}
func extractKey(flags []string) string {
	for _, f := range flags {
		if strings.HasPrefix(f, "--remote_header=x-buildbuddy-api-key=") {
			return strings.TrimPrefix(f, "--remote_header=x-buildbuddy-api-key=")
		}
	}
	return "<no-key>"
}
func redact(flags []string) []string {
	result := append([]string(nil), flags...)
	for i, f := range result {
		if strings.HasPrefix(f, "--remote_header=x-buildbuddy-api-key=") {
			result[i] = "--remote_header=x-buildbuddy-api-key=<fixture-key>"
		}
	}
	return result
}

func workspace(t *testing.T, data []byte, delay bool) string {
	ws := t.TempDir()
	write(t, filepath.Join(ws, "WORKSPACE"), nil)
	write(t, filepath.Join(ws, "payload"), data)
	if err := os.Symlink("payload", filepath.Join(ws, "input-link")); err != nil {
		t.Fatal(err)
	}
	sleep := ""
	if delay {
		sleep = "sleep 12; "
	}
	write(t, filepath.Join(ws, "BUILD"), []byte(fmt.Sprintf(`genrule(name="middle", srcs=["input-link"], outs=["middle.bin"], cmd="%scat $(location input-link) > $@")
genrule(name="final", srcs=[":middle"], outs=["result.bin", "result.link"], cmd="cat $(location :middle) > $(location result.bin); ln -s result.bin $(location result.link)")
`, sleep)))
	return ws
}
func snapshot(t *testing.T, ws string) [][]byte {
	t.Helper()
	var result [][]byte
	for _, name := range []string{"result.bin", "result.link", "middle.bin"} {
		b, err := os.ReadFile(filepath.Join(ws, "bazel-bin", name))
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, b)
	}
	link, err := os.Readlink(filepath.Join(ws, "bazel-bin/result.link"))
	if err != nil {
		t.Fatal(err)
	}
	result = append(result, []byte(link))
	return result
}
func compare(t *testing.T, ws string, expected [][]byte) {
	t.Helper()
	got := snapshot(t, ws)
	for i := range got {
		if !bytes.Equal(got[i], expected[i]) {
			t.Fatalf("seed=%d output=%d differs from local reference", *seed, i)
		}
	}
}

func TestRandomizedFlags(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	if *iterations < 1 || *iterations > 24 {
		t.Fatal("flags_iterations must be 1..24")
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy}})
	bin := bazel(t)
	rng := rand.New(rand.NewSource(*seed))
	for i := 0; i < *iterations; i++ {
		t.Run(fmt.Sprintf("case_%02d", i), func(t *testing.T) {
			data := make([]byte, rng.Intn(8192))
			rng.Read(data)
			ws := workspace(t, data, false)
			build(t, bin, ws, nil)
			expected := snapshot(t, ws)
			component := harness.App
			if i%2 == 1 {
				component = harness.CacheProxy
			}
			instance := []string{"", "flags/nested", "flags-with-dash", "flags/seed/83142"}[i%4]
			flags := []string{"--remote_cache=grpc://" + f.Endpoint(component).GRPC, "--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--remote_header=x-flags-probe=seeded", "--remote_instance_name=" + instance, fmt.Sprintf("--experimental_remote_merkle_tree_cache=%t", rng.Intn(2) == 0), fmt.Sprintf("--remote_local_fallback=%t", rng.Intn(2) == 0), fmt.Sprintf("--remote_retries=%d", rng.Intn(4)), "--remote_timeout=30", fmt.Sprintf("--experimental_remote_cache_eviction_retries=%d", rng.Intn(3)), "--remote_download_outputs=minimal", "--remote_download_regex=.*\\.(bin|link)$"}
			if component == harness.App {
				flags = append(flags, "--remote_executor=grpc://"+f.Endpoint(harness.App).GRPC, "--spawn_strategy=remote", "--remote_default_exec_properties=workload-isolation-type=none", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64")
			}
			t.Logf("seed=%d flags=%v", *seed, redact(flags))
			// Fresh output roots force both execution/upload and a subsequent real AC hit.
			build(t, bin, ws, flags)
			compare(t, ws, expected)
			out := build(t, bin, ws, flags)
			compare(t, ws, expected)
			if !strings.Contains(out, "remote cache hit") {
				t.Fatalf("seed=%d warm build did not hit remote cache:\n%s", *seed, out)
			}
		})
	}
}

// Find only the daemon that owns this fixture container, including private
// harness daemons. No other container is stopped or restarted.
func dockerFor(t *testing.T, container string) []string {
	t.Helper()
	candidates := [][]string{nil}
	sockets, err := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sockets {
		candidates = append(candidates, []string{"--host", "unix://" + s})
	}
	for _, prefix := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		args := append(append([]string(nil), prefix...), "inspect", container)
		err := exec.CommandContext(ctx, "docker", args...).Run()
		cancel()
		if err == nil {
			return prefix
		}
	}
	t.Fatal("fixture Docker daemon not found")
	return nil
}
func TestRestartDuringBuild(t *testing.T) {
	t.Logf("seed=%d deterministic restart case", *seed)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Args: map[harness.Component][]string{harness.Executor: {"--app.log_level=debug"}}})
	bin := bazel(t)
	ws := workspace(t, []byte("restart-output\x00\xff"), true)
	build(t, bin, ws, nil)
	expected := snapshot(t, ws)
	prefix := dockerFor(t, f.Endpoint(harness.App).Container)
	flags := []string{"--remote_cache=grpc://" + f.Endpoint(harness.App).GRPC, "--remote_executor=grpc://" + f.Endpoint(harness.App).GRPC, "--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--spawn_strategy=remote", "--remote_local_fallback", "--remote_retries=8", "--remote_timeout=3", "--remote_default_exec_properties=workload-isolation-type=none", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64"}
	// Begin the outage once Bazel has submitted work, as seen in executor's log.
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
		defer cancel()
		for {
			args := append(append([]string(nil), prefix...), "logs", f.Endpoint(harness.Executor).Container)
			b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
			if err != nil {
				done <- err
				return
			}
			if strings.Contains(string(b), "Executing command") || strings.Contains(string(b), "Executing task") {
				break
			}
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		args := append(append([]string(nil), prefix...), "restart", "--time=0", f.Endpoint(harness.App).Container)
		b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			err = fmt.Errorf("restart: %w %s", err, b)
		}
		done <- err
	}()
	build(t, bin, ws, flags)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	compare(t, ws, expected)
}
