package workspaces_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

var seed = flag.Int64("workspaces.seed", 314, "Replay seed")
var iterations = flag.Int("workspaces.iterations", 2, "Number of source edits (0..12)")

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
}

func bazel(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://releases.bazel.build/7.4.1/release/bazel-7.4.1-linux-x86_64", nil)
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
	if hex.EncodeToString(sum[:]) != "c97f02133adce63f0c28678ac1f21d65fa8255c80429b588aeeba8a1fac6202b" {
		t.Fatal("Bazel checksum mismatch")
	}
	path := filepath.Join(t.TempDir(), "bazel")
	write(t, path, string(b))
	return path
}

// Every action concatenates its inputs, so content changes propagate exactly
// through the graph. No timestamps or random values occur inside actions.
func TestRandomWorkspaces(t *testing.T) {
	t.Logf("replay: -workspaces.seed=%d -workspaces.iterations=%d", *seed, *iterations)
	if *iterations < 0 || *iterations > 12 {
		t.Fatal("iterations must be 0..12")
	}
	r := rand.New(rand.NewSource(*seed))
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy}})
	bin := bazel(t)
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(p, 0755)
			}
			return nil
		})
	})
	ws := filepath.Join(root, "workspace")
	if err := os.Mkdir(ws, 0755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "WORKSPACE"), "workspace(name = \"random_workspace\")\n")
	write(t, filepath.Join(ws, "tree.bzl"), `def _tree(ctx):
    out = ctx.actions.declare_directory(ctx.label.name + ".dir")
    ctx.actions.run_shell(inputs = ctx.files.srcs, outputs = [out], command = "mkdir -p " + out.path + "/nested; cat " + " ".join([f.path for f in ctx.files.srcs]) + " > " + out.path + "/nested/payload; ln -s nested/payload " + out.path + "/link", mnemonic = "WorkspaceTree")
    return [DefaultInfo(files = depset([out]))]
tree = rule(implementation = _tree, attrs = {"srcs": attr.label_list(allow_files = True)})
`)
	const sources = 3
	contents := make([]string, sources)
	for i := range contents {
		contents[i] = fmt.Sprintf("seed=%d source=%d value=%016x\n", *seed, i, r.Uint64())
		write(t, filepath.Join(ws, fmt.Sprintf("s%d.txt", i)), contents[i])
	}
	n := 6 + r.Intn(4)
	deps := make([][]int, n)
	var build strings.Builder
	build.WriteString("load(\":tree.bzl\", \"tree\")\n")
	for i := 0; i < n; i++ {
		// Negative indices identify sources. Always include a private source,
		// plus chain and diamond edges, and randomized older dependencies.
		deps[i] = []int{-1 - i%sources}
		if i > 0 {
			deps[i] = append(deps[i], i-1)
		}
		if i > 1 && (i == 2 || r.Intn(2) == 0) {
			deps[i] = append(deps[i], r.Intn(i-1))
		}
		var labels []string
		for _, d := range deps[i] {
			if d < 0 {
				labels = append(labels, fmt.Sprintf("\"s%d.txt\"", -d-1))
			} else {
				labels = append(labels, fmt.Sprintf("\":n%d\"", d))
			}
		}
		fmt.Fprintf(&build, "genrule(name=\"n%d\", srcs=[%s], outs=[\"n%d.out\"], cmd=\"cat $(SRCS) > $@\")\n", i, strings.Join(labels, ","), i)
	}
	fmt.Fprintf(&build, "tree(name=\"tree\", srcs=[\":n%d\"])\n", n-1)
	build.WriteString("sh_binary(name=\"reader\", srcs=[\"reader.sh\"], data=[\":tree\"])\nsh_test(name=\"check\", srcs=[\"check.sh\"], data=[\":tree\", \":reader\"])\n")
	write(t, filepath.Join(ws, "BUILD"), build.String())
	write(t, filepath.Join(ws, "reader.sh"), "#!/bin/sh\ncat random_workspace/tree.dir/link\n")
	write(t, filepath.Join(ws, "check.sh"), "#!/bin/sh\nset -eu\ncd \"$TEST_SRCDIR\"\nrandom_workspace/reader > \"$TEST_TMPDIR/actual\"\ncmp \"$TEST_TMPDIR/actual\" random_workspace/tree.dir/nested/payload\n")
	t.Logf("graph: %v", deps)
	type mode struct {
		name, endpoint string
		execution      bool
	}
	modes := []mode{{name: "local"}, {name: "cache", endpoint: f.Endpoint(harness.App).GRPC}, {name: "cache-replay", endpoint: f.Endpoint(harness.App).GRPC}, {name: "direct", endpoint: f.Endpoint(harness.App).GRPC, execution: true}, {name: "proxy", endpoint: f.Endpoint(harness.CacheProxy).GRPC, execution: true}}
	// Strip dispatch credentials and Bazel runfiles environment from nested tools.
	var env []string
	for _, e := range os.Environ() {
		k := strings.SplitN(e, "=", 2)[0]
		if k == "BUILDBUDDY_API_KEY" || strings.HasPrefix(k, "BB_") || strings.HasPrefix(k, "TEST_") || strings.HasPrefix(k, "RUNFILES") || k == "JAVA_RUNFILES" {
			continue
		}
		env = append(env, e)
	}
	for _, m := range modes {
		base := filepath.Join(root, m.name)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c := exec.CommandContext(ctx, bin, "--output_base="+base, "shutdown")
			c.Dir = ws
			c.Env = env
			_ = c.Run()
		})
	}
	for step := 0; step <= *iterations+1; step++ {
		changed := -1
		if step > 0 && step <= *iterations {
			changed = r.Intn(sources)
			contents[changed] += fmt.Sprintf("edit=%d value=%016x\n", step, r.Uint64())
			write(t, filepath.Join(ws, fmt.Sprintf("s%d.txt", changed)), contents[changed])
		}
		affected := make([]bool, n)
		expected := []string{}
		for i, ds := range deps {
			for _, d := range ds {
				if step == 0 || (d < 0 && -d-1 == changed) || (d >= 0 && affected[d]) {
					affected[i] = true
				}
			}
			if affected[i] {
				expected = append(expected, fmt.Sprintf("//:n%d", i))
			}
		}
		if affected[n-1] {
			expected = append(expected, "//:tree")
		}
		sort.Strings(expected)
		var reference map[string]string
		for _, m := range modes {
			logPath := filepath.Join(root, fmt.Sprintf("%s-%d.json", m.name, step))
			args := []string{"--output_base=" + filepath.Join(root, m.name), "--host_jvm_args=-Xmx256m", "--ignore_all_rc_files", "test", "//...", "--enable_bzlmod=false", "--jobs=2", "--local_resources=cpu=2", "--test_output=errors", "--remote_download_outputs=all", "--execution_log_json_file=" + logPath, "--spawn_strategy=local", "--genrule_strategy=local"}
			if m.endpoint != "" {
				args = append(args, "--remote_cache=grpc://"+m.endpoint, "--remote_header=x-buildbuddy-api-key="+f.APIKey, "--remote_instance_name=rand-workspaces-"+strings.TrimSuffix(m.name, "-replay"))
			}
			if m.execution {
				args = append(args, "--remote_executor=grpc://"+f.Endpoint(harness.App).GRPC, "--spawn_strategy=remote", "--genrule_strategy=remote", "--remote_accept_cached=false", "--remote_default_exec_properties=workload-isolation-type=none", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			c := exec.CommandContext(ctx, bin, args...)
			c.Dir = ws
			c.Env = env
			out, err := c.CombinedOutput()
			cancel()
			if err != nil {
				t.Fatalf("seed=%d step=%d mode=%s: %v\n%s", *seed, step, m.name, err, out)
			}
			logs, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			// Preserve nested execution evidence without command-line credentials.
			if dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); dir != "" {
				write(t, filepath.Join(dir, fmt.Sprintf("workspace-%s-%d.json", m.name, step)), string(logs))
			}
			decoder := json.NewDecoder(bytes.NewReader(logs))
			actual := []string{}
			for {
				var entry struct {
					Mnemonic    string
					TargetLabel string
					CacheHit    bool
					Runner      string
				}
				err := decoder.Decode(&entry)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if entry.Mnemonic == "Genrule" || entry.Mnemonic == "WorkspaceTree" {
					actual = append(actual, entry.TargetLabel)
					if m.name == "cache-replay" && (!entry.CacheHit || entry.Runner != "remote cache hit") {
						t.Fatalf("seed=%d step=%d cache replay %s did not hit remote cache: %+v", *seed, step, entry.TargetLabel, entry)
					}
				}
			}
			sort.Strings(actual)
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("seed=%d step=%d mode=%s rebuild set: got %v want %v", *seed, step, m.name, actual, expected)
			}
			outputs := map[string]string{}
			for i := 0; i < n; i++ {
				p := fmt.Sprintf("n%d.out", i)
				b, err := os.ReadFile(filepath.Join(ws, "bazel-bin", p))
				if err != nil {
					t.Fatal(err)
				}
				outputs[p] = string(b)
			}
			for _, p := range []string{"tree.dir/nested/payload", "tree.dir/link"} {
				b, err := os.ReadFile(filepath.Join(ws, "bazel-bin", p))
				if err != nil {
					t.Fatal(err)
				}
				outputs[p] = string(b)
			}
			target, err := os.Readlink(filepath.Join(ws, "bazel-bin/tree.dir/link"))
			if err != nil {
				t.Fatal(err)
			}
			outputs["symlink-target"] = target
			if m.name == "local" {
				reference = outputs
			} else if !reflect.DeepEqual(outputs, reference) {
				t.Fatalf("seed=%d step=%d mode=%s outputs differ from local", *seed, step, m.name)
			}
			t.Logf("seed=%d step=%d source=%d mode=%s rebuilt=%v outputs agree", *seed, step, changed, m.name, actual)
		}
	}
}
