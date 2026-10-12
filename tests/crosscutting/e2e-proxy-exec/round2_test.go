package chain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
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

type buildResult struct {
	out, log string
	output   []byte
	err      error
}

func TestRound2(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy, harness.Executor}, Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"cache_target": "grpc://cacheproxy:1985"}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	bazel := bazelClient(t, ctx)
	workspace, install := t.TempDir(), filepath.Join(t.TempDir(), "install")
	write := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(workspace, name), []byte(content), 0644))
	}
	write("WORKSPACE", "workspace(name = \"chain2\")\n")
	write("MODULE.bazel", "module(name = \"chain2\")\n")
	marker := "/tmp/" + f.ID + "-started"
	write("outputs.bzl", `def _impl(ctx):
    tree = ctx.actions.declare_directory(ctx.label.name + ".tree")
    link = ctx.actions.declare_file(ctx.label.name + ".link")
    ctx.actions.run_shell(inputs = [ctx.file.src], outputs = [tree, link], command = "mkdir -p %s/nested; cp %s %s/nested/data; touch %s/empty; ln -s nested/data %s/relative; ln -s %s/nested/data %s" % (tree.path, ctx.file.src.path, tree.path, tree.path, tree.path, tree.basename, link.path))
    return [DefaultInfo(files = depset([tree, link]))]
outputs = rule(implementation = _impl, attrs = {"src": attr.label(allow_single_file = True)})
`)
	write("BUILD.bazel", fmt.Sprintf(`load(":outputs.bzl", "outputs")
genrule(name="shared", srcs=["input"], outs=["shared.bin"], cmd="sleep 3; cat $(location input) > $@")
genrule(name="left", srcs=[":shared"], outs=["left.bin"], cmd="cat $(location :shared) > $@; echo left >> $@")
genrule(name="right", srcs=[":shared"], outs=["right.bin"], cmd="cat $(location :shared) > $@; echo right >> $@")
genrule(name="slow", srcs=["input"], outs=["slow.bin"], cmd="touch %s; sleep 15; cat $(location input) > $@")
outputs(name="structured", src="input")
`, marker))
	write("input", strings.Repeat("binary\x00payload\xff\n", 10000))
	// Locate only the daemon owning this fixture, including the harness-owned fallback.
	hosts := []string{""}
	sockets, err := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	require.NoError(t, err)
	for _, s := range sockets {
		hosts = append(hosts, "unix://"+s)
	}
	var dockerPrefix []string
	found := false
	for _, host := range hosts {
		prefix := []string{}
		if host != "" {
			prefix = []string{"--host", host}
		}
		if exec.CommandContext(ctx, "docker", append(prefix, "inspect", f.Endpoint(harness.App).Container)...).Run() == nil {
			dockerPrefix = prefix
			found = true
			break
		}
	}
	require.True(t, found, "cannot locate fixture Docker daemon")
	docker := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", append(append([]string{}, dockerPrefix...), args...)...).CombinedOutput()
	}
	command := func(out string, targets []string, cached bool) *exec.Cmd {
		args := []string{"--batch", "--bazelrc=/dev/null", "--output_base=" + out, "--install_base=" + install, "build"}
		args = append(args, targets...)
		args = append(args, "--enable_bzlmod=false", "--enable_workspace=true", "--incompatible_autoload_externally=", "--spawn_strategy=remote", "--genrule_strategy=remote", "--remote_cache=grpc://"+f.Endpoint(harness.CacheProxy).GRPC, "--remote_executor=grpc://"+f.Endpoint(harness.App).GRPC, "--remote_header=x-buildbuddy-api-key="+f.APIKey, "--remote_instance_name=round2", "--remote_timeout=120s", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64", "--remote_default_exec_properties=workload-isolation-type=none", "--remote_download_outputs=all", "--remote_local_fallback=false", fmt.Sprintf("--remote_accept_cached=%t", cached), "--execution_log_json_file="+filepath.Join(out, "execution.json"), "--jobs=4", "--color=no", "--curses=no")
		cmd := exec.CommandContext(ctx, bazel, args...)
		cmd.Dir = workspace
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + workspace, "TMPDIR=" + os.TempDir()}
		return cmd
	}
	run := func(out string, targets []string, cached bool) buildResult {
		output, err := command(out, targets, cached).CombinedOutput()
		return buildResult{out: out, log: filepath.Join(out, "execution.json"), output: output, err: err}
	}
	check := func(t *testing.T, r buildResult, cache *bool) map[string]string {
		require.NoError(t, r.err, "%s", r.output)
		data, err := os.ReadFile(r.log)
		require.NoError(t, err)
		decoder := json.NewDecoder(bytes.NewReader(data))
		count := 0
		for decoder.More() {
			var e struct {
				Runner         string
				CacheHit       bool
				RemoteCacheHit bool
				ExitCode       int
			}
			require.NoError(t, decoder.Decode(&e))
			require.Contains(t, e.Runner, "remote")
			require.Zero(t, e.ExitCode)
			if cache != nil {
				require.Equal(t, *cache, e.CacheHit || e.RemoteCacheHit, "%s", data)
			}
			count++
		}
		require.Positive(t, count)
		root := filepath.Join(r.out, "execroot", "chain2", "bazel-out", "k8-fastbuild", "bin")
		snapshot := map[string]string{}
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if d.Type()&os.ModeSymlink != 0 {
				target, e := os.Readlink(path)
				snapshot[rel] = "link:" + target
				return e
			}
			if d.IsDir() {
				snapshot[rel] = "dir"
				return nil
			}
			b, e := os.ReadFile(path)
			snapshot[rel] = "file:" + string(b)
			return e
		}))
		return snapshot
	}
	yes, no := true, false
	verify := func(t *testing.T, targets []string) {
		baseline := check(t, run(t.TempDir(), targets, false), &no)
		cached := check(t, run(t.TempDir(), targets, true), &yes)
		require.Equal(t, baseline, cached, "fresh cached rebuild differs from no-cache execution")
	}
	t.Run("concurrent-same", func(t *testing.T) {
		outs := []string{t.TempDir(), t.TempDir()}
		results := make(chan buildResult, 2)
		for _, out := range outs {
			go func(out string) { results <- run(out, []string{"//:left"}, true) }(out)
		}
		a, b := check(t, <-results, nil), check(t, <-results, nil)
		require.Equal(t, a, b)
		verify(t, []string{"//:left"})
	})
	t.Run("concurrent-overlap", func(t *testing.T) {
		outs := []string{t.TempDir(), t.TempDir()}
		results := make(chan buildResult, 2)
		for i, out := range outs {
			target := []string{"//:left", "//:right"}[i]
			go func(out, target string) { results <- run(out, []string{target}, false) }(out, target)
		}
		a, b := check(t, <-results, &no), check(t, <-results, &no)
		require.Equal(t, a["shared.bin"], b["shared.bin"])
		verify(t, []string{"//:left", "//:right"})
	})
	t.Run("cancel-rebuild", func(t *testing.T) {
		cmd := command(t.TempDir(), []string{"//:slow"}, false)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		require.NoError(t, cmd.Start())
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		require.Eventually(t, func() bool {
			_, err := docker("exec", f.Endpoint(harness.Executor).Container, "test", "-f", marker)
			return err == nil
		}, 90*time.Second, 250*time.Millisecond, "action never started")
		require.NoError(t, cmd.Process.Signal(os.Interrupt))
		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Fatal("Bazel failed to exit after SIGINT")
		}
		verify(t, []string{"//:slow"})
	})
	t.Run("directory-symlink", func(t *testing.T) { verify(t, []string{"//:structured"}) })
	for _, component := range []harness.Component{harness.CacheProxy, harness.App, harness.Executor} {
		t.Run("restart-"+string(component), func(t *testing.T) {
			targets := []string{"//:left", "//:right", "//:structured"}
			before := check(t, run(t.TempDir(), targets, false), &no)
			output, err := docker("restart", f.Endpoint(component).Container)
			require.NoError(t, err, "%s", output)
			client := &http.Client{Timeout: time.Second}
			require.Eventually(t, func() bool {
				r, err := client.Get(f.Endpoint(component).HTTP + "/readyz?server-type=" + f.Endpoint(component).ServerType)
				if err != nil {
					return false
				}
				r.Body.Close()
				return r.StatusCode == 200
			}, 90*time.Second, 500*time.Millisecond)
			require.Equal(t, before, check(t, run(t.TempDir(), targets, true), &yes))
			verify(t, targets)
		})
	}
}
