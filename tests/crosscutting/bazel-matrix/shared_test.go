package bazelmatrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

// Each client has its own workspace and output root. Identical relative action
// paths and input bytes permit cache reuse without sharing a local action cache.
func TestSharedProxyCache(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	const seed int64 = 20261012
	t.Logf("payload seed: %d", seed)
	rng := rand.New(rand.NewSource(seed))
	payload := make([]byte, 4096)
	if _, err := rng.Read(payload); err != nil {
		t.Fatal(err)
	}
	binaries := make([]string, len(bazelVersions))
	for i, v := range bazelVersions {
		binaries[i] = download(t, writableTemp(t), v.version, v.hash)
	}
	seenDigests := make(map[string]bool)
	client := func(t *testing.T, index int, phase, instance string, hit bool) string {
		t.Helper()
		ws, root := writableTemp(t), writableTemp(t)
		write(t, filepath.Join(ws, "MODULE.bazel"), "module(name = \"matrix\")\n")
		write(t, filepath.Join(ws, "WORKSPACE"), "")
		write(t, filepath.Join(ws, "rules.bzl"), rule)
		write(t, filepath.Join(ws, "BUILD.bazel"), "load(\":rules.bzl\", \"matrix\")\nmatrix(name = \"tree\", src = \"input\")\n")
		write(t, filepath.Join(ws, "input"), string(payload))
		args := []string{"--batch", "--output_user_root=" + root, "--host_jvm_args=-Xmx256m", "--ignore_all_rc_files", "build", "//:tree", "--jobs=1", "--execution_log_json_file=" + filepath.Join(f.OutputDir, phase+".actions.json"), "--enable_bzlmod", "--remote_timeout=60", "--remote_download_outputs=all", "--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--remote_instance_name=" + instance, "--remote_cache=grpc://" + f.Endpoint(harness.CacheProxy).GRPC, "--spawn_strategy=local"}
		if index > 0 {
			args = append(args, "--incompatible_autoload_externally=")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, binaries[index], args...)
		cmd.Dir = ws
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + ws, "TMPDIR=" + root, "LANG=C.UTF-8"}
		log, err := cmd.CombinedOutput()
		write(t, filepath.Join(f.OutputDir, phase+".log"), string(log))
		if err != nil {
			t.Fatalf("%s: %v\n%s", phase, err, log)
		}
		// Compare REAPI action identities: a Bazel version may serialize a
		// different Command for the same Starlark action. Such a miss is valid.
		actionLog, err := os.Open(filepath.Join(f.OutputDir, phase+".actions.json"))
		if err != nil {
			t.Fatal(err)
		}
		defer actionLog.Close()
		decoder := json.NewDecoder(actionLog)
		digest := ""
		for {
			var entry struct {
				Mnemonic string `json:"mnemonic"`
				Digest   struct {
					Hash string `json:"hash"`
				} `json:"digest"`
			}
			err := decoder.Decode(&entry)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if entry.Mnemonic == "MatrixTree" {
				digest = entry.Digest.Hash
			}
		}
		if digest == "" {
			t.Fatal("missing MatrixTree action digest")
		}
		t.Logf("%s action digest: %s", phase, digest)
		// The map is populated only by sequential builds, before parallel waves.
		known := instance == "matrix-shared" && seenDigests[digest]
		if (hit || known) && !strings.Contains(string(log), "remote cache hit") {
			t.Fatalf("%s: no remote cache hit\n%s", phase, log)
		}
		out := filepath.Join(ws, "bazel-bin", "tree")
		for _, name := range []string{"nested/value", "link"} {
			got, err := os.ReadFile(filepath.Join(out, name))
			if err != nil || string(got) != string(payload) {
				t.Fatalf("%s: incorrect %s: %v", phase, name, err)
			}
		}
		empty, err := os.ReadFile(filepath.Join(out, "empty"))
		if err != nil || len(empty) != 0 {
			t.Fatalf("empty: %v (%d bytes)", err, len(empty))
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
		return digest
	}
	for i, v := range bazelVersions {
		t.Run("sequential-"+v.version, func(t *testing.T) {
			digest := client(t, i, "sequential-"+v.version, "matrix-shared", false)
			seenDigests[digest] = true
		})
	}
	t.Run("bazel-7-after-upgrades", func(t *testing.T) {
		client(t, 0, "bazel-7-after-upgrades", "matrix-shared", true)
	})
	t.Run("concurrent-cold", func(t *testing.T) {
		for i, v := range bazelVersions {
			t.Run(v.version, func(t *testing.T) {
				t.Parallel()
				client(t, i, "concurrent-cold-"+v.version, "matrix-concurrent-cold", false)
			})
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		for i, v := range bazelVersions {
			t.Run(v.version, func(t *testing.T) {
				t.Parallel()
				client(t, i, fmt.Sprintf("concurrent-%s", v.version), "matrix-shared", true)
			})
		}
	})
}
