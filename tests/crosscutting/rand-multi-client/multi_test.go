package multiclient_test

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

var seed = flag.Int64("multi.seed", 314159, "deterministic workspace and schedule seed")
var rounds = flag.Int("multi.iterations", 3, "edit/build rounds (bounded by test deadline)")

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

// graph always includes a chain and a diamond; additional edges vary by seed.
func graph(r *rand.Rand) (string, int) {
	n := 6 + r.Intn(5)
	var b strings.Builder
	for i := 0; i < n; i++ {
		srcs := []string{"input.bin"}
		if i > 0 {
			srcs = []string{fmt.Sprintf("n%d.bin", i-1)}
		}
		if i == 2 {
			srcs = []string{"n0.bin"}
		}
		if i == 3 {
			srcs = []string{"n1.bin", "n2.bin"}
		}
		if i > 3 && r.Intn(2) == 0 {
			srcs = append(srcs, fmt.Sprintf("n%d.bin", r.Intn(i-1)))
		}
		quoted := make([]string, len(srcs))
		locations := make([]string, len(srcs))
		for j, s := range srcs {
			quoted[j] = fmt.Sprintf("%q", s)
			locations[j] = "$(location " + s + ")"
		}
		fmt.Fprintf(&b, "genrule(name = %q, srcs = [%s], outs = [%q], cmd = %q)\n", fmt.Sprintf("n%d", i), strings.Join(quoted, ","), fmt.Sprintf("n%d.bin", i), "cat "+strings.Join(locations, " ")+" > $@; printf 'node-"+fmt.Sprint(i)+"\\n' >> $@")
	}
	return b.String(), n
}

type client struct{ workspace, output, install string }

func TestConcurrentBazelClients(t *testing.T) {
	t.Logf("replay: -multi.seed=%d -multi.iterations=%d", *seed, *rounds)
	require.Greater(t, *rounds, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy}})
	bazel := bazelClient(t, ctx)
	r := rand.New(rand.NewSource(*seed))
	build, nodes := graph(r)
	count := 3 + r.Intn(4)
	t.Logf("seed=%d clients=%d nodes=%d", *seed, count, nodes)
	newClient := func() client {
		c := client{t.TempDir(), filepath.Join(t.TempDir(), "out"), filepath.Join(t.TempDir(), "install")}
		require.NoError(t, os.WriteFile(filepath.Join(c.workspace, "WORKSPACE"), []byte("workspace(name = \"multi\")\n"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(c.workspace, "BUILD.bazel"), []byte(build), 0644))
		return c
	}
	solo := newClient()
	clients := make([]client, count)
	for i := range clients {
		clients[i] = newClient()
	}
	run := func(c client, remote bool, proxy bool, clean bool, delay time.Duration) ([][]byte, error) {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if clean {
			if err := os.RemoveAll(c.output); err != nil {
				return nil, err
			}
		}
		args := []string{"--batch", "--bazelrc=/dev/null", "--output_base=" + c.output, "--install_base=" + c.install,
			"--host_jvm_args=-Xmx256m", "--host_jvm_args=-XX:ActiveProcessorCount=2", "build", "//...",
			"--enable_bzlmod=false", "--enable_workspace=true", "--incompatible_autoload_externally=", "--jobs=2", "--loading_phase_threads=2", "--color=no", "--curses=no"}
		logPath := filepath.Join(c.workspace, "execution.json")
		if remote {
			endpoint := f.Endpoint(harness.App).GRPC
			if proxy {
				endpoint = f.Endpoint(harness.CacheProxy).GRPC
			}
			args = append(args, "--spawn_strategy=remote", "--genrule_strategy=remote", "--remote_cache=grpc://"+endpoint,
				"--remote_executor=grpc://"+f.Endpoint(harness.App).GRPC, "--remote_header=x-buildbuddy-api-key="+f.APIKey,
				"--remote_instance_name=multi", "--remote_timeout=60s", "--remote_local_fallback=false", "--remote_download_outputs=all",
				"--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64",
				"--remote_default_exec_properties=workload-isolation-type=none", "--execution_log_json_file="+logPath)
		} else {
			args = append(args, "--spawn_strategy=local", "--genrule_strategy=local")
		}
		cmd := exec.CommandContext(ctx, bazel, args...)
		cmd.Dir = c.workspace
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + c.workspace, "TMPDIR=" + os.TempDir()}
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("bazel: %w\n%s", err, output)
		}
		if remote {
			data, err := os.ReadFile(logPath)
			if err != nil {
				return nil, err
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			spawns := 0
			for decoder.More() {
				var entry struct {
					Mnemonic, Runner string
					ExitCode         int
				}
				if err := decoder.Decode(&entry); err != nil {
					return nil, err
				}
				if entry.Mnemonic == "Genrule" {
					spawns++
				}
				if entry.Mnemonic == "Genrule" && (!strings.Contains(entry.Runner, "remote") || entry.ExitCode != 0) {
					return nil, fmt.Errorf("unexpected execution: %+v", entry)
				}
			}
			if clean && spawns != nodes {
				return nil, fmt.Errorf("clean build recorded %d genrules, expected %d", spawns, nodes)
			}
		}
		result := make([][]byte, nodes)
		for i := range result {
			result[i], err = os.ReadFile(filepath.Join(c.output, "execroot", "multi", "bazel-out", "k8-fastbuild", "bin", fmt.Sprintf("n%d.bin", i)))
			if err != nil {
				return nil, err
			}
		}
		return result, nil
	}
	// Generate state transitions before starting goroutines, so scheduling cannot change replay.
	initial := make([]byte, 4096+r.Intn(4096))
	_, _ = r.Read(initial)
	states := [][]byte{initial}
	for round := 1; round < *rounds; round++ {
		payload := append([]byte(nil), initial...)
		if round%3 != 2 {
			for j := 0; j < 1+r.Intn(16); j++ {
				payload[r.Intn(len(payload))] = byte(r.Intn(256))
			}
		}
		states = append(states, payload)
	}
	for round, payload := range states {
		require.NoError(t, os.WriteFile(filepath.Join(solo.workspace, "input.bin"), payload, 0644))
		expected, err := run(solo, false, false, true, 0)
		require.NoError(t, err, "seed=%d round=%d solo", *seed, round)
		wants := make([][][]byte, count)
		delays := make([]time.Duration, count)
		cleans := make([]bool, count)
		for i, c := range clients {
			state := payload
			// Two clients keep an older state while others edit, then all converge on revert.
			if round%3 == 1 && i >= count-2 {
				state = initial
			}
			require.NoError(t, os.WriteFile(filepath.Join(c.workspace, "input.bin"), state, 0644))
			wants[i] = expected
			if !bytes.Equal(state, payload) {
				require.NoError(t, os.WriteFile(filepath.Join(solo.workspace, "input.bin"), state, 0644))
				wants[i], err = run(solo, false, false, true, 0)
				require.NoError(t, err)
			}
			delays[i] = time.Duration(r.Intn(80)) * time.Millisecond
			cleans[i] = round == 0 || round%3 == 2 || r.Intn(2) == 0
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, c := range clients {
			wg.Add(1)
			go func(i int, c client) {
				defer wg.Done()
				<-start
				actual, err := run(c, true, i%2 == 1, cleans[i], delays[i])
				if err != nil {
					t.Errorf("seed=%d round=%d client=%d proxy=%v clean=%v: %v", *seed, round, i, i%2 == 1, cleans[i], err)
					return
				}
				for node := range actual {
					if !bytes.Equal(wants[i][node], actual[node]) {
						t.Errorf("seed=%d round=%d client=%d node=%d output mismatch: expected sha256=%x actual sha256=%x", *seed, round, i, node, sha256.Sum256(wants[i][node]), sha256.Sum256(actual[node]))
					}
				}
			}(i, c)
		}
		close(start)
		wg.Wait()
		if t.Failed() {
			return
		}
		t.Logf("seed=%d round=%d: all %d clients match solo outputs", *seed, round, count)
	}
}
