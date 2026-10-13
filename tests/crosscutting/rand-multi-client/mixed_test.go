package multiclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

// A dependency-free Starlark rule works on all three Bazel major versions.
// Hostname is a separate output: payload bytes have an independent Go oracle,
// while hostname proves even cache hits originated in the fixture executor.
const mixedRule = `def _impl(ctx):
    out = ctx.outputs.out
    host = ctx.outputs.host
    ctx.actions.run_shell(
        inputs = ctx.files.srcs,
        outputs = [out, host],
        command = ctx.attr.prefix + "cat " + " ".join([f.path for f in ctx.files.srcs]) + " > " + out.path + "; printf 'node-%s\\n' >> %s; hostname > %s" % (ctx.attr.node, out.path, host.path),
        mnemonic = "MultiClient",
    )
    return [DefaultInfo(files = depset([out, host]))]
node = rule(implementation = _impl, attrs = {
    "srcs": attr.label_list(allow_files = True),
    "out": attr.output(mandatory = True),
    "host": attr.output(mandatory = True),
    "node": attr.string(),
    "prefix": attr.string(),
})
`

type mixedResult struct {
	output []byte
	err    error
}

func TestMixedVersionsCancellation(t *testing.T) {
	require.Greater(t, *rounds, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	signals := t.TempDir()
	require.NoError(t, os.Chmod(signals, 0777))
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy},
		DockerArgs: map[harness.Component][]string{harness.Executor: {"--volume", signals + ":/multi-signals"}},
	})
	versions := []string{"7.6.1", "8.4.2", "9.0.0"}
	binaries := make([]string, len(versions))
	for i, v := range versions {
		binaries[i] = versionedBazelClient(t, ctx, v)
	}
	r := rand.New(rand.NewSource(*seed))
	nodes := 6 + r.Intn(5)
	edges := make([][]int, nodes)
	for i := 1; i < nodes; i++ {
		edges[i] = []int{i - 1}
		if i == 2 {
			edges[i] = []int{0}
		}
		if i == 3 {
			edges[i] = []int{1, 2}
		}
		if i > 3 && r.Intn(2) == 0 {
			edges[i] = append(edges[i], r.Intn(i-1))
		}
	}
	clients := make([]client, 6)
	for i := range clients {
		clients[i] = client{t.TempDir(), filepath.Join(t.TempDir(), "out"), filepath.Join(t.TempDir(), "install")}
		require.NoError(t, os.WriteFile(filepath.Join(clients[i].workspace, "MODULE.bazel"), []byte("module(name = \"multi\")\n"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(clients[i].workspace, "rules.bzl"), []byte(mixedRule), 0644))
	}
	t.Logf("replay: -multi.seed=%d -multi.iterations=%d; versions=%v nodes=%d edges=%v", *seed, *rounds, versions, nodes, edges)
	for round := 0; round < *rounds; round++ {
		// Fresh random inputs ensure the marker cannot be skipped by a prior cache hit.
		payload := make([]byte, 4096+r.Intn(4096))
		_, _ = r.Read(payload)
		wants := make([][]byte, nodes)
		var build strings.Builder
		build.WriteString("load(\":rules.bzl\", \"node\")\n")
		marker := fmt.Sprintf("round-%d", round)
		for n, deps := range edges {
			srcs := []string{"input.bin"}
			wants[n] = append([]byte(nil), payload...)
			if len(deps) > 0 {
				srcs = nil
				wants[n] = nil
				for _, d := range deps {
					srcs = append(srcs, fmt.Sprintf("n%d.bin", d))
					wants[n] = append(wants[n], wants[d]...)
				}
			}
			wants[n] = append(wants[n], []byte(fmt.Sprintf("node-%d\n", n))...)
			quoted := make([]string, len(srcs))
			for j, s := range srcs {
				quoted[j] = fmt.Sprintf("%q", s)
			}
			prefix := ""
			if n == 0 {
				prefix = "touch /multi-signals/" + marker + "; sleep 20; "
			}
			fmt.Fprintf(&build, "node(name=%q, srcs=[%s], node=%q, prefix=%q, out=%q, host=%q)\n", fmt.Sprintf("n%d", n), strings.Join(quoted, ","), fmt.Sprint(n), prefix, fmt.Sprintf("n%d.bin", n), fmt.Sprintf("n%d.host", n))
		}
		for _, c := range clients {
			require.NoError(t, os.WriteFile(filepath.Join(c.workspace, "BUILD.bazel"), []byte(build.String()), 0644))
			require.NoError(t, os.WriteFile(filepath.Join(c.workspace, "input.bin"), payload, 0644))
			require.NoError(t, os.RemoveAll(c.output))
		}
		// Cover each major version in the first three rounds; seed chooses its route.
		victim := (round%3)*2 + r.Intn(2)
		delay := time.Duration(500+r.Intn(1500)) * time.Millisecond
		t.Logf("round=%d victim=%d version=%s proxy=%v cancellation-delay=%s", round, victim, versions[victim/2], victim%2 == 1, delay)
		launch := func(i int, runCtx context.Context, phase string) <-chan mixedResult {
			done := make(chan mixedResult, 1)
			c := clients[i]
			endpoint := f.Endpoint(harness.App).GRPC
			if i%2 == 1 {
				endpoint = f.Endpoint(harness.CacheProxy).GRPC
			}
			logPath := filepath.Join(c.workspace, "execution.json")
			args := []string{"--batch", "--ignore_all_rc_files", "--output_base=" + c.output, "--install_base=" + c.install,
				"--host_jvm_args=-Xmx256m", "--host_jvm_args=-XX:ActiveProcessorCount=2", "build", "//...", "--enable_bzlmod",
				"--jobs=2", "--loading_phase_threads=2", "--color=no", "--curses=no",
				"--spawn_strategy=remote", "--remote_cache=grpc://" + endpoint,
				"--remote_executor=grpc://" + f.Endpoint(harness.App).GRPC, "--remote_header=x-buildbuddy-api-key=" + f.APIKey,
				"--remote_instance_name=mixed-multi", "--remote_timeout=90s", "--remote_local_fallback=false", "--remote_download_outputs=all",
				"--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64",
				"--remote_default_exec_properties=workload-isolation-type=none", "--execution_log_json_file=" + logPath}
			if i/2 > 0 {
				args = append(args, "--incompatible_autoload_externally=")
			}
			go func() {
				cmd := exec.CommandContext(runCtx, binaries[i/2], args...)
				cmd.Dir = c.workspace
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Dir(c.output), "TMPDIR=" + os.TempDir(), "LANG=C.UTF-8"}
				// SIGINT exercises Bazel's RPC cancellation; WaitDelay bounds graceful shutdown.
				cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
				cmd.WaitDelay = 15 * time.Second
				output, err := cmd.CombinedOutput()
				artifact := filepath.Join(f.OutputDir, fmt.Sprintf("mixed-round-%d-client-%d-%s.log", round, i, phase))
				if writeErr := os.WriteFile(artifact, output, 0644); writeErr != nil && err == nil {
					err = writeErr
				}
				done <- mixedResult{output, err}
			}()
			return done
		}
		check := func(i int, result mixedResult) {
			t.Helper()
			require.NoError(t, result.err, "round=%d client=%d version=%s proxy=%v\n%s", round, i, versions[i/2], i%2 == 1, result.output)
			c := clients[i]
			for n, want := range wants {
				got, err := os.ReadFile(filepath.Join(c.workspace, "bazel-bin", fmt.Sprintf("n%d.bin", n)))
				require.NoError(t, err)
				require.Equal(t, want, got, "round=%d client=%d node=%d", round, i, n)
				host, err := os.ReadFile(filepath.Join(c.workspace, "bazel-bin", fmt.Sprintf("n%d.host", n)))
				require.NoError(t, err)
				require.Equal(t, f.Endpoint(harness.Executor).Container, strings.TrimSpace(string(host)), "output must originate in fixture executor")
			}
			data, err := os.ReadFile(filepath.Join(c.workspace, "execution.json"))
			require.NoError(t, err)
			dec := json.NewDecoder(bytes.NewReader(data))
			spawns := 0
			for dec.More() {
				var e struct {
					Mnemonic, Runner string
					ExitCode         int
				}
				require.NoError(t, dec.Decode(&e))
				if e.Mnemonic == "MultiClient" {
					spawns++
					require.Contains(t, e.Runner, "remote")
					require.Zero(t, e.ExitCode)
				}
			}
			require.Equal(t, nodes, spawns, "fresh client must record every action")
		}
		victimCtx, stopVictim := context.WithCancel(ctx)
		victimDone := launch(victim, victimCtx, "cancelled")
		// Since only the victim has started, this marker proves its cold action began.
		deadline := time.NewTimer(90 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		started := false
		for !started {
			select {
			case result := <-victimDone:
				stopVictim()
				deadline.Stop()
				ticker.Stop()
				t.Fatalf("victim exited before remote action marker: %v\n%s", result.err, result.output)
			case <-deadline.C:
				stopVictim()
				ticker.Stop()
				t.Fatal("remote action did not start within 90s")
			case <-ctx.Done():
				stopVictim()
				deadline.Stop()
				ticker.Stop()
				t.Fatal(ctx.Err())
			case <-ticker.C:
				_, err := os.Stat(filepath.Join(signals, marker))
				started = err == nil
			}
		}
		deadline.Stop()
		ticker.Stop()
		results := make([]<-chan mixedResult, len(clients))
		for i := range clients {
			if i != victim {
				results[i] = launch(i, ctx, "survivor")
			}
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
		stopVictim()
		cancelled := <-victimDone
		// Drain all clients before any fatal assertion, so cleanup cannot race them.
		completed := make([]mixedResult, len(clients))
		for i, done := range results {
			if done != nil {
				completed[i] = <-done
			}
		}
		require.Error(t, cancelled.err, "victim must be interrupted during its remote build")
		var exitErr *exec.ExitError
		require.ErrorAs(t, cancelled.err, &exitErr)
		require.Equal(t, 8, exitErr.ExitCode(), "Bazel must report its interrupted exit code, not an unrelated failure: %s", cancelled.output)
		t.Logf("round=%d victim cancellation: %v", round, cancelled.err)
		for i := range clients {
			if i != victim {
				check(i, completed[i])
			}
		}
		require.NoError(t, os.RemoveAll(clients[victim].output))
		check(victim, <-launch(victim, ctx, "recovery"))
		t.Logf("round=%d: five survivors and cancelled-client recovery match oracle", round)
	}
}
