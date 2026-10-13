package bazeltestrbe

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

var seed = flag.Int64("bazel_test_seed", 314008, "Replay seed")
var iterations = flag.Int("bazel_test_iterations", 3, "Bounded randomized cases (1..12)")

func put(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestBazelTestProperties(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	if *iterations < 1 || *iterations > 12 {
		t.Fatal("iterations must be 1..12")
	}
	// This binary is only executed inside the outer bb remote test runner.
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	dir := t.TempDir()
	t.Cleanup(func() {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				os.Chmod(path, 0755)
			}
			return nil
		})
	})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://releases.bazel.build/8.4.2/release/bazel-8.4.2-linux-x86_64", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("Bazel download: %s", resp.Status)
	}
	binary, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(binary)) != "4dc8e99dfa802e252dac176d08201fd15c542ae78c448c8a89974b6f387c282c" {
		t.Fatal("Bazel checksum mismatch")
	}
	bazel := filepath.Join(dir, "bazel")
	put(t, bazel, string(binary))
	toolDir := filepath.Join(dir, "toolchain")
	if err := os.MkdirAll(toolDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file", "zip"} {
		put(t, filepath.Join(toolDir, name), string(helper))
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, DockerArgs: map[harness.Component][]string{harness.Executor: {"--volume", toolDir + ":" + toolDir + ":ro"}}})
	rng := rand.New(rand.NewSource(*seed))
	for iteration := 0; iteration < *iterations; iteration++ {
		shards := 1 + rng.Intn(4)
		payload := fmt.Sprintf("seed-%d-case-%d-value-%016x", *seed, iteration, rng.Uint64())
		t.Run(fmt.Sprintf("seed_%d_case_%d_shards_%d", *seed, iteration, shards), func(t *testing.T) {
			ws := filepath.Join(dir, fmt.Sprintf("ws%d", iteration))
			if err := os.MkdirAll(ws, 0755); err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(ws, "MODULE.bazel"), "module(name = \"test_properties\")\n")
			if err := os.MkdirAll(filepath.Join(ws, "tools"), 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"zip", "file", "lcovmerge"} {
				put(t, filepath.Join(ws, "tools", name), string(helper))
			}
			// A dependency-free Starlark test avoids downloading rules_shell/toolchains.
			put(t, filepath.Join(ws, "test.bzl"), `def _impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(out, ctx.attr.script, is_executable = True)
    return [DefaultInfo(executable = out, runfiles = ctx.runfiles(files = ctx.files.tools)), coverage_common.instrumented_files_info(ctx, source_attributes = ["srcs"])]
def _merge(ctx):
    out = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(output = out, target_file = ctx.file.bin, is_executable = True)
    return [DefaultInfo(executable = out)]
merge_tool = rule(implementation = _merge, executable = True, attrs = {"bin": attr.label(allow_single_file = True)})
fixture_test = rule(implementation = _impl, test = True, attrs = {"script": attr.string(), "tools": attr.label_list(allow_files = True), "srcs": attr.label_list(allow_files = True)})
`)
			script := fmt.Sprintf(`#!/bin/sh
set -eu
export PATH="$TEST_SRCDIR/$TEST_WORKSPACE/tools:$PATH"
[ -z "${TEST_SHARD_STATUS_FILE:-}" ] || touch "$TEST_SHARD_STATUS_FILE"
case "${FEATURE:-pass}" in
 fail) echo EXPECTED_FAILURE; exit 1 ;;
 timeout) echo BEFORE_TIMEOUT; sleep 20; echo AFTER_TIMEOUT; exit 0 ;;
esac
i=${TEST_SHARD_INDEX:-0}
if [ "${COVERAGE:-0}" = 1 ]; then
 printf 'SF:fixture.sh\nDA:1,1\nLF:1\nLH:1\nend_of_record\n' > "$COVERAGE_OUTPUT_FILE"
fi
printf 'MARKER %s shard=%%s total=%%s\n' "$i" "${TEST_TOTAL_SHARDS:-1}"
mkdir -p "$TEST_UNDECLARED_OUTPUTS_DIR/nested"
printf '%s:%%s' "$i" > "$TEST_UNDECLARED_OUTPUTS_DIR/nested/payload.txt"
printf '<testsuites><testsuite name="fixture" tests="1" failures="0"><testcase name="shard_%%s"/></testsuite></testsuites>\n' "$i" > "$XML_OUTPUT_FILE"
`, payload, payload)
			put(t, filepath.Join(ws, "BUILD.bazel"), fmt.Sprintf("load(\":test.bzl\", \"fixture_test\", \"merge_tool\")\nmerge_tool(name = \"lcovmerge\", bin = \"tools/lcovmerge\")\nfixture_test(name = \"sample\", shard_count = %d, srcs = [\"fixture.sh\"], tools = glob([\"tools/*\"]), script = %q)\n", shards, script))
			// Cleanup must run even if an assertion fails.
			defer func() {
				for _, mode := range []string{"local", "remote", "remote-fresh"} {
					stopCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
					cmd := exec.CommandContext(stopCtx, bazel, "--output_user_root="+filepath.Join(dir, fmt.Sprintf("root%d_%s", iteration, mode)), "--ignore_all_rc_files", "shutdown")
					cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
					cmd.Dir = ws
					cmd.CombinedOutput()
					stop()
				}
			}()
			call := 0
			put(t, filepath.Join(ws, "fixture.sh"), "#!/bin/sh\nexit 0\n")
			run := func(mode string, remote bool, extra ...string) string {
				t.Helper()
				root := filepath.Join(dir, fmt.Sprintf("root%d_%s", iteration, mode))
				args := []string{"--output_user_root=" + root, "--host_jvm_args=-Xmx256m", "--ignore_all_rc_files", "test", "//:sample", "--enable_bzlmod", "--jobs=4", "--test_output=errors", "--test_sharding_strategy=explicit", "--remote_download_outputs=all", "--zip_undeclared_test_outputs", "--color=no", "--curses=no", "--test_env=PATH=" + toolDir + ":/usr/bin:/bin"}
				if remote {
					args = append(args, "--remote_executor=grpc://"+f.Endpoint(harness.App).GRPC, "--remote_cache=grpc://"+f.Endpoint(harness.App).GRPC, "--remote_header=x-buildbuddy-api-key="+f.APIKey, "--remote_default_exec_properties=workload-isolation-type=none", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64", "--spawn_strategy=remote", "--remote_local_fallback=false")
				} else {
					args = append(args, "--spawn_strategy=local")
				}
				for _, arg := range extra {
					if arg == "--combined_report=lcov" {
						args[3] = "coverage"
					}
				}
				args = append(args, extra...)
				cmd := exec.CommandContext(ctx, bazel, args...)
				cmd.Dir = ws
				// Never inherit the outer dispatch credential or Bazel's test-only environment.
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "USER=runner", "TMPDIR=" + dir}
				out, err := cmd.CombinedOutput()
				call++
				if saveErr := os.WriteFile(filepath.Join(f.OutputDir, fmt.Sprintf("case%d-%s-%d.log", iteration, mode, call)), out, 0600); saveErr != nil {
					t.Fatal(saveErr)
				}
				expectFailure := false
				for _, arg := range extra {
					if arg == "--test_env=FEATURE=fail" || arg == "--test_env=FEATURE=timeout" {
						expectFailure = true
					}
				}
				if expectFailure {
					exit, ok := err.(*exec.ExitError)
					if !ok || exit.ExitCode() != 3 {
						t.Fatalf("seed=%d: expected Bazel test-failure exit 3, got %v\n%s", *seed, err, out)
					}
				} else if err != nil {
					t.Fatalf("seed=%d mode=%s: %v\n%s", *seed, mode, err, out)
				}
				return string(out)
			}
			run("local", false)
			local := artifacts(t, ws)
			out := run("remote", true)
			if !strings.Contains(out, "remote") {
				t.Fatalf("seed=%d: no remote execution reported:\n%s", *seed, out)
			}
			remote := artifacts(t, ws)
			if fmt.Sprint(local) != fmt.Sprint(remote) {
				t.Fatalf("seed=%d: local=%v remote=%v", *seed, local, remote)
			}
			if len(remote) != shards*3 {
				t.Fatalf("seed=%d: expected log/XML/archive per shard, got %v", *seed, remote)
			}
			fresh := run("remote-fresh", true)
			if !strings.Contains(fresh, "(cached)") || fmt.Sprint(artifacts(t, ws)) != fmt.Sprint(local) {
				t.Fatalf("seed=%d: fresh-client cache/artifact mismatch:\n%s", *seed, fresh)
			}
			cached := run("remote", true)
			if !strings.Contains(cached, "(cached)") {
				t.Fatalf("seed=%d: test not reused:\n%s", *seed, cached)
			}
			rerun := run("remote", true, "--cache_test_results=no")
			if strings.Contains(rerun, "(cached)") {
				t.Fatalf("seed=%d: no-cache test reused", *seed)
			}
			run("local", false, "--test_output=streamed", "--cache_test_results=no")
			localStreamed := artifacts(t, ws)
			streamed := run("remote", true, "--test_output=streamed", "--cache_test_results=no")
			if !strings.Contains(streamed, "MARKER "+payload) {
				t.Fatalf("seed=%d: streamed output missing:\n%s", *seed, streamed)
			}
			// Streamed tests intentionally execute locally without sharding in Bazel 8.
			if actual := artifacts(t, ws); fmt.Sprint(actual) != fmt.Sprint(localStreamed) {
				t.Fatalf("seed=%d: streamed artifacts differ: local=%v actual=%v", *seed, localStreamed, actual)
			}
			if len(localStreamed) != 3 {
				t.Fatalf("seed=%d: streamed did not return one log/XML/archive: %v", *seed, localStreamed)
			}
			// Synthetic shell instrumentation isolates transport and aggregation from language toolchains.
			var reference string
			for _, remoteMode := range []bool{false, true} {
				mode := "local"
				if remoteMode {
					mode = "remote"
				}
				run(mode, remoteMode, "--combined_report=lcov", "--coverage_report_generator=//:lcovmerge", "--instrument_test_targets", "--cache_test_results=no")
				report, err := os.ReadFile(filepath.Join(ws, "bazel-out", "_coverage", "_coverage_report.dat"))
				if err != nil {
					t.Fatal(err)
				}
				expected := fmt.Sprintf("SF:fixture.sh\nDA:1,%d\nLF:1\nLH:1\nend_of_record\n", shards)
				if string(report) != expected {
					t.Fatalf("seed=%d: coverage expected %q got %q", *seed, expected, report)
				}
				if !remoteMode {
					reference = string(report)
				} else if string(report) != reference {
					t.Fatalf("seed=%d: coverage differs", *seed)
				}
			}
			// Failure and timeout probes are bounded and compare Bazel's classification.
			for _, remoteMode := range []bool{false, true} {
				mode := "local"
				if remoteMode {
					mode = "remote"
				}
				failure := run(mode, remoteMode, "--test_env=FEATURE=fail", "--flaky_test_attempts=3", "--test_sharding_strategy=disabled", "--cache_test_results=no")
				if !strings.Contains(failure, "FAILED") || !strings.Contains(failure, "EXPECTED_FAILURE") {
					t.Fatalf("seed=%d: failure output missing:\n%s", *seed, failure)
				}
				// Bazel retains the first two attempts separately and the final one as test.log.
				for attempt := 1; attempt <= 2; attempt++ {
					path := filepath.Join(ws, "bazel-testlogs", "sample", "test_attempts", fmt.Sprintf("attempt_%d.log", attempt))
					data, err := os.ReadFile(path)
					if err != nil || !strings.Contains(string(data), "EXPECTED_FAILURE") {
						t.Fatalf("seed=%d: missing retry %d: %v", *seed, attempt, err)
					}
				}
				timeout := run(mode, remoteMode, "--test_env=FEATURE=timeout", "--test_timeout=2", "--test_sharding_strategy=disabled", "--cache_test_results=no")
				if !strings.Contains(timeout, "TIMEOUT") || strings.Contains(timeout, "AFTER_TIMEOUT") {
					t.Fatalf("seed=%d: wrong timeout result:\n%s", *seed, timeout)
				}
			}
		})
	}
}

// Normalize only relative artifact names. XML and test logs are deliberately
// deterministic, so byte equality detects missing or changed remote outputs.
func artifacts(t *testing.T, ws string) []string {
	t.Helper()
	base := filepath.Join(ws, "bazel-testlogs", "sample")
	var result []string
	_, singleErr := os.Stat(filepath.Join(base, "test.log"))
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Bazel leaves old sharded logs behind when streamed output disables sharding.
			if singleErr == nil && strings.HasPrefix(info.Name(), "shard_") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		if info.Name() == "test.xml" || info.Name() == "test.log" {
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			result = append(result, rel+":"+string(b))
		}
		if info.Name() == "outputs.zip" {
			z, e := zip.OpenReader(path)
			if e != nil {
				return e
			}
			defer z.Close()
			for _, entry := range z.File {
				if entry.FileInfo().IsDir() {
					continue
				}
				r, e := entry.Open()
				if e != nil {
					return e
				}
				b, e := io.ReadAll(r)
				r.Close()
				if e != nil {
					return e
				}
				result = append(result, rel+":"+entry.Name+":"+string(b))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// The bare executor is not a full Bazel toolchain image. These narrow helpers
// supply the two tools test-setup.sh needs for undeclared outputs using a
// read-only fixture mount and declared runfiles. They access no external endpoint.
func init() {
	switch filepath.Base(os.Args[0]) {
	case "file":
		fmt.Println("application/octet-stream")
		os.Exit(0)
	case "lcovmerge":
		if err := mergeCoverage(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "zip":
		if err := zipOutputs(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func zipOutputs(args []string) error {
	if len(args) < 3 || args[0] != "-qr" {
		return fmt.Errorf("unexpected zip arguments: %v", args)
	}
	out, err := os.Create(args[1])
	if err != nil {
		return err
	}
	z := zip.NewWriter(out)
	for _, path := range args[2:] {
		if path == "--" {
			continue
		}
		err = filepath.Walk(path, func(name string, info os.FileInfo, e error) error {
			if e != nil {
				return e
			}
			if info.IsDir() {
				return nil
			}
			w, e := z.Create(filepath.ToSlash(name))
			if e != nil {
				return e
			}
			r, e := os.Open(name)
			if e != nil {
				return e
			}
			defer r.Close()
			_, e = io.Copy(w, r)
			return e
		})
		if err != nil {
			z.Close()
			out.Close()
			return err
		}
	}
	if err = z.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// The synthetic fixture has one source line; a simple reference merger sums
// that line across shards, without Java or a language coverage toolchain.
func mergeCoverage(args []string) error {
	output, reports := "", ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "--output_file=") {
			output = strings.TrimPrefix(arg, "--output_file=")
		}
		if strings.HasPrefix(arg, "--reports_file=") {
			reports = strings.TrimPrefix(arg, "--reports_file=")
		}
	}
	if output == "" || reports == "" {
		return fmt.Errorf("unexpected merger args: %v", args)
	}
	manifest, err := os.ReadFile(reports)
	if err != nil {
		return err
	}
	sum := 0
	for _, path := range strings.Fields(string(manifest)) {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "DA:1,") {
				n, err := strconv.Atoi(strings.TrimPrefix(line, "DA:1,"))
				if err != nil {
					return err
				}
				sum += n
			}
		}
	}
	return os.WriteFile(output, []byte(fmt.Sprintf("SF:fixture.sh\nDA:1,%d\nLF:1\nLH:1\nend_of_record\n", sum)), 0644)
}
