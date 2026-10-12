package bazelmatrix

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

const rule = `def _impl(ctx):
    out = ctx.actions.declare_directory(ctx.label.name)
    ctx.actions.run_shell(
        inputs = [ctx.file.src],
        outputs = [out],
        command = "mkdir -p '%s/nested'; cp '%s' '%s/nested/value'; touch '%s/empty'; ln -s nested/value '%s/link'; printf '#!/bin/sh\\nexit 0\\n' > '%s/executable'; chmod +x '%s/executable'; hostname > '%s/host'" % (out.path, ctx.file.src.path, out.path, out.path, out.path, out.path, out.path, out.path),
        mnemonic = "MatrixTree",
        execution_requirements = {},
    )
    return [DefaultInfo(files = depset([out]))]
matrix = rule(implementation = _impl, attrs = {"src": attr.label(allow_single_file = True)})
`

// Bazel deliberately makes output tree directories read-only. Register this
// after TempDir so the LIFO cleanup restores permissions before Go removes it.
func writableTemp(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0755)
			}
			return nil
		})
		if err != nil {
			t.Errorf("restore temporary directory permissions: %v", err)
		}
	})
	return root
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

// Download actual release binaries, with fixed hashes rather than a mutable
// Bazelisk selection. Only the nested build's fixture key enters its environment.
func download(t *testing.T, dir, version, hash string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://releases.bazel.build/"+version+"/release/bazel-"+version+"-linux-x86_64", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("Bazel download: %s", res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 150<<20))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != hash {
		t.Fatalf("Bazel %s checksum: %s", version, got)
	}
	path := filepath.Join(dir, "bazel-"+version)
	if err := os.WriteFile(path, b, 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBazelMatrix(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy}})
	versions := []struct{ version, hash string }{
		{"7.6.1", "ac6249d1192aea9feaf49dfee2ab50c38cee2454b00cf29bbec985a11795c025"},
		{"8.4.2", "4dc8e99dfa802e252dac176d08201fd15c542ae78c448c8a89974b6f387c282c"},
		{"9.0.0", "c44a93f25398c68f904fa1d19b61d321de6c0d2f09dca375d7bc0dc9b9428403"},
	}
	for _, v := range versions {
		binary := download(t, writableTemp(t), v.version, v.hash)
		for _, mode := range []string{"cache", "execution"} {
			t.Run(v.version+"/"+mode, func(t *testing.T) {
				ws := writableTemp(t)
				write(t, filepath.Join(ws, "MODULE.bazel"), "module(name = \"matrix\")\n")
				write(t, filepath.Join(ws, "WORKSPACE"), "")
				write(t, filepath.Join(ws, "rules.bzl"), rule)
				write(t, filepath.Join(ws, "BUILD.bazel"), "load(\":rules.bzl\", \"matrix\")\nmatrix(name = \"tree\", src = \"input\")\n")
				initial := "initial " + v.version + " " + mode + "\n"
				write(t, filepath.Join(ws, "input"), initial)
				incrementalRoot := writableTemp(t)
				run := func(phase string, remote bool, want string, hit bool) {
					t.Helper()
					root := incrementalRoot
					if strings.Contains(phase, "hit") {
						root = writableTemp(t)
					}
					args := []string{"--batch", "--output_user_root=" + root, "--host_jvm_args=-Xmx256m", "--ignore_all_rc_files", "build", "//:tree", "--jobs=2", "--enable_bzlmod", "--remote_timeout=60", "--remote_download_outputs=all", "--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--remote_instance_name=matrix-" + v.version + "-" + mode, "--remote_cache=grpc://" + f.Endpoint(harness.CacheProxy).GRPC}
					if !strings.HasPrefix(v.version, "7.") {
						args = append(args, "--incompatible_autoload_externally=")
					}
					if remote {
						args = append(args, "--remote_executor=grpc://"+f.Endpoint(harness.App).GRPC, "--spawn_strategy=remote", "--remote_default_exec_properties=workload-isolation-type=none", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64")
					} else {
						args = append(args, "--spawn_strategy=local")
					}
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					cmd := exec.CommandContext(ctx, binary, args...)
					cmd.Dir = ws
					// Explicit allowlist: never inherit dispatch credentials, outer Bazel flags,
					// or remote endpoints from the runner environment.
					cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + ws, "TMPDIR=" + root, "LANG=C.UTF-8"}
					log, err := cmd.CombinedOutput()
					write(t, filepath.Join(f.OutputDir, strings.ReplaceAll(v.version+"-"+mode+"-"+phase, "/", "_")+".log"), string(log))
					if err != nil {
						t.Fatalf("%s: %v\n%s", phase, err, log)
					}
					if hit && !strings.Contains(string(log), "remote cache hit") {
						t.Fatalf("%s did not use remote cache:\n%s", phase, log)
					}
					out := filepath.Join(ws, "bazel-bin", "tree")
					for _, name := range []string{"nested/value", "link"} {
						got, err := os.ReadFile(filepath.Join(out, name))
						if err != nil || string(got) != want {
							t.Fatalf("%s %s: %q %v, want %q", phase, name, got, err, want)
						}
					}
					b, err := os.ReadFile(filepath.Join(out, "empty"))
					if err != nil || len(b) != 0 {
						t.Fatalf("empty: %q %v", b, err)
					}
					target, err := os.Readlink(filepath.Join(out, "link"))
					if err != nil || target != "nested/value" {
						t.Fatalf("symlink: %q %v", target, err)
					}
					info, err := os.Stat(filepath.Join(out, "executable"))
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode()&0111 == 0 {
						t.Fatal("lost executable permission")
					}
					if mode == "execution" {
						host, err := os.ReadFile(filepath.Join(out, "host"))
						if err != nil || strings.TrimSpace(string(host)) != f.Endpoint(harness.Executor).Container {
							t.Fatalf("action did not run in fixture executor: %q %v", host, err)
						}
					}
				}
				remote := mode == "execution"
				run("cold", remote, initial, false)
				run("no-op", remote, initial, false)
				run("fresh-client-hit", remote, initial, true)
				updated := "edited\n\x00binary\xff\n" + initial
				write(t, filepath.Join(ws, "input"), updated)
				run("edited", remote, updated, false)
				run("edited-hit", remote, updated, true)
				write(t, filepath.Join(ws, "input"), initial)
				run("reverted-hit", remote, initial, true)
			})
		}
	}
}
