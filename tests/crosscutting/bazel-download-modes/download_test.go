package downloadmodes_test

import (
	"context"
	"crypto/sha256"
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
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func write(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0600))
}

// Pin and verify the standalone client; it runs only on the hosted test runner.
func bazelClient(t *testing.T) string {
	return bazelVersion(t, "8.4.2")
}

func bazelVersion(t *testing.T, version string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://releases.bazel.build/%s/release/bazel-%s-linux-x86_64", version, version), nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	require.NoError(t, err)
	checksums := map[string]string{"7.6.1": "ac6249d1192aea9feaf49dfee2ab50c38cee2454b00cf29bbec985a11795c025", "8.4.2": "4dc8e99dfa802e252dac176d08201fd15c542ae78c448c8a89974b6f387c282c", "8.5.0": "18255229d933b8da10151bdef223a302744296b09af8af1988c93faa1ea3c71f"}
	require.Equal(t, checksums[version], fmt.Sprintf("%x", sha256.Sum256(b)))
	path := filepath.Join(t.TempDir(), "bazel")
	require.NoError(t, os.WriteFile(path, b, 0700))
	return path
}

// Pass only ordinary process settings; no dispatch credentials enter Bazel.
func clientEnv() []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "USER", "TMPDIR", "TEST_TMPDIR", "JAVA_HOME", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func TestBazelDownloadModes(t *testing.T) {
	for _, version := range []string{"7.6.1", "8.4.2", "8.5.0"} {
		t.Run(version, func(t *testing.T) { testDownloadModes(t, version) })
	}
}

func testDownloadModes(t *testing.T, version string) {
	bin := bazelVersion(t, version)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy}})
	workspace := t.TempDir()
	write(t, filepath.Join(workspace, "WORKSPACE"), "workspace(name = \"download_modes\")\n")
	write(t, filepath.Join(workspace, "BUILD"), `genrule(name="intermediate", outs=["intermediate.txt"], cmd="printf 'download-mode-payload\\n' > $@")
genrule(name="final", srcs=[":intermediate"], outs=["final.txt"], cmd="cat $(location :intermediate) > $@")
`)
	// Each output base starts cold, so a previous local materialization cannot
	// accidentally satisfy a subsequent download-policy assertion.
	cases := []struct {
		mode   string
		remote bool
	}{{"minimal", true}, {"toplevel", true}, {"all", true}, {"all", false}}
	for _, scenario := range cases {
		mode := scenario.mode
		for _, disk := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/remote-exec=%t/disk=%t", mode, scenario.remote, disk), func(t *testing.T) {
				base := t.TempDir()
				args := []string{"--batch", "--output_base=" + base, "--ignore_all_rc_files", "build", "//:final", "--enable_bzlmod=false", "--jobs=2", "--remote_cache=grpc://" + f.Endpoint(harness.CacheProxy).GRPC, "--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--remote_download_outputs=" + mode, asyncFlag(version), "--remote_instance_name=" + fmt.Sprintf("%s-%t-%t", mode, scenario.remote, disk), "--remote_timeout=60", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64", "--remote_default_exec_properties=workload-isolation-type=none"}
				args = append(args, workspaceFlags(version)...)
				if scenario.remote {
					args = append(args, "--remote_executor=grpc://"+f.Endpoint(harness.App).GRPC)
				}
				if disk {
					args = append(args, "--disk_cache="+t.TempDir())
				}
				run := func() string {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
					defer cancel()
					cmd := exec.CommandContext(ctx, bin, args...)
					cmd.Dir = workspace
					cmd.Env = clientEnv()
					out, err := cmd.CombinedOutput()
					require.NoError(t, err, "%s", out)
					return string(out)
				}
				first := run()
				t.Log(first)
				output := filepath.Join(base, "execroot", "download_modes", "bazel-out", "k8-fastbuild", "bin")
				assertFile := func(name string, want bool) {
					b, err := os.ReadFile(filepath.Join(output, name))
					if !want {
						require.True(t, os.IsNotExist(err), "%s should not be downloaded: %v", name, err)
						return
					}
					require.NoError(t, err)
					require.Equal(t, "download-mode-payload\n", string(b))
				}
				assertFile("intermediate.txt", mode == "all")
				assertFile("final.txt", mode != "minimal")
				// Fresh action graph, same remote and optional disk caches. Async uploads
				// must be complete when the batch client exits.
				args[1] = "--output_base=" + t.TempDir()
				second := run()
				t.Log(second)
				require.Contains(t, second, "cache hit", "cold output base should reuse cached results")
				output = filepath.Join(strings.TrimPrefix(args[1], "--output_base="), "execroot", "download_modes", "bazel-out", "k8-fastbuild", "bin")
				assertFile("intermediate.txt", mode == "all")
				assertFile("final.txt", mode != "minimal")
			})
		}
	}
}

// Construct the precise post-eviction state (AC entry exists, output bytes do
// not) without depending on the Pebble LRU maintenance interval.
func TestActionCacheMissingOutputBytes(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		for _, kind := range []string{"file", "nested-tree", "tree-blob"} {
			t.Run(string(component)+"/"+kind, func(t *testing.T) {
				c := f.Clients(component)
				instance := "missing-output-" + string(component) + "-" + kind
				command, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf output > out"}, OutputPaths: []string{"out"}})
				require.NoError(t, err)
				root, err := c.UploadProto(ctx, instance, &repb.Directory{})
				require.NoError(t, err)
				action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: command, InputRootDigest: root})
				require.NoError(t, err)
				missingBytes := []byte("not-uploaded-" + instance)
				missing := harness.Digest(missingBytes)
				result := &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out", Digest: missing}}}
				if kind != "file" {
					leaf := &repb.Directory{Files: []*repb.FileNode{{Name: "leaf", Digest: missing}}}
					_, leafDigest, err := harness.MarshalDigest(leaf)
					require.NoError(t, err)
					tree := &repb.Tree{Root: &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "nested", Digest: leafDigest}}}, Children: []*repb.Directory{leaf}}
					treeBytes, treeDigest, err := harness.MarshalDigest(tree)
					require.NoError(t, err)
					if kind == "nested-tree" {
						_, err = c.Upload(ctx, instance, treeBytes)
						require.NoError(t, err)
					} else {
						// Here the leaf exists but the Tree message itself was evicted.
						_, err = c.Upload(ctx, instance, missingBytes)
						require.NoError(t, err)
						missingBytes, missing = treeBytes, treeDigest
					}
					result = &repb.ActionResult{OutputDirectories: []*repb.OutputDirectory{{Path: "out", TreeDigest: treeDigest}}}
				}
				_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: action, ActionResult: result})
				require.NoError(t, err)
				_, err = c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
				require.Equal(t, codes.NotFound, status.Code(err), "dangling AC entry must not be returned as a usable hit")
				_, err = c.Upload(ctx, instance, missingBytes)
				require.NoError(t, err)
				restored, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
				require.NoError(t, err)
				require.True(t, proto.Equal(result, &repb.ActionResult{OutputFiles: restored.OutputFiles, OutputDirectories: restored.OutputDirectories}))
				b, err := c.Read(ctx, instance, missing)
				require.NoError(t, err)
				require.Equal(t, missingBytes, b)
			})
		}
	}
}

func TestExecutionRecoversMissingCachedOutput(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	instance := "execution-missing-output"
	payload := []byte("regenerated output\n")
	output := harness.Digest(payload)
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	command, err := c.UploadProto(ctx, instance, &repb.Command{
		Arguments:   []string{"/bin/sh", "-c", "printf 'regenerated output\\n' > out"},
		OutputPaths: []string{"out"},
		Platform:    &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}},
	})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: command, InputRootDigest: root})
	require.NoError(t, err)
	_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: action, ActionResult: &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out", Digest: output}}}})
	require.NoError(t, err)
	missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{output}})
	require.NoError(t, err)
	require.Len(t, missing.MissingBlobDigests, 1)
	for _, cached := range []bool{false, true} {
		stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action})
		require.NoError(t, err)
		var response *repb.ExecuteResponse
		for {
			op, err := stream.Recv()
			require.NoError(t, err, "execution ended before a completed operation")
			if !op.Done {
				continue
			}
			require.Nil(t, op.GetError())
			response = &repb.ExecuteResponse{}
			require.NoError(t, op.GetResponse().UnmarshalTo(response))
			break
		}
		require.Zero(t, response.GetStatus().GetCode(), "%v", response.GetStatus())
		require.Zero(t, response.GetResult().GetExitCode())
		require.Equal(t, cached, response.CachedResult, "first execution must rebuild; second must reuse restored bytes")
		require.Len(t, response.Result.OutputFiles, 1)
		require.True(t, proto.Equal(output, response.Result.OutputFiles[0].Digest))
		b, err := c.Read(ctx, instance, output)
		require.NoError(t, err)
		require.Equal(t, payload, b)
	}
}

// Exercise Bazel's persisted BWoB metadata against actual Pebble eviction,
// rather than just testing an artificially dangling action-cache entry.
func TestBazelMinimalRecoversAfterEviction(t *testing.T) {
	bin := bazelClient(t)
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy},
		Config: map[harness.Component]harness.Config{
			harness.CacheProxy: {"cache": harness.Config{"max_size_bytes": int64(8 << 20), "pebble": harness.Config{"min_eviction_age": "0s", "atime_update_threshold": "0s", "sampler_iter_refresh_peroid": "1s", "presence_cache": harness.Config{"max_entries": 0}}}},
			harness.App:        {"cache": harness.Config{"max_size_bytes": int64(8 << 20), "pebble": harness.Config{"min_eviction_age": "0s", "atime_update_threshold": "0s", "sampler_iter_refresh_peroid": "1s", "presence_cache": harness.Config{"max_entries": 0}}}},
		},
	})
	workspace, base := t.TempDir(), t.TempDir()
	write(t, filepath.Join(workspace, "WORKSPACE"), "workspace(name = \"eviction\")\n")
	write(t, filepath.Join(workspace, "BUILD"), `genrule(name="out", outs=["out.txt"], cmd="printf 'eviction-payload\\n' > $@")
genrule(name="later", srcs=[":out"], outs=["later.txt"], cmd="cat $(location :out) > $@", local=True)`)
	args := []string{"--batch", "--output_base=" + base, "--ignore_all_rc_files", "build", "//:out", "--enable_bzlmod=false", "--jobs=2", "--remote_cache=grpc://" + f.Endpoint(harness.CacheProxy).GRPC, "--remote_executor=grpc://" + f.Endpoint(harness.App).GRPC, "--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--remote_instance_name=eviction", "--remote_timeout=60", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64", "--remote_default_exec_properties=workload-isolation-type=none"}
	run := func(mode string) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append(args, "--remote_download_outputs="+mode)...)
		cmd.Dir = workspace
		cmd.Env = clientEnv()
		b, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", b)
		t.Log(string(b))
	}
	args = append(args, workspaceFlags("8.4.2")...)
	run("minimal")
	localOutput := filepath.Join(base, "execroot", "eviction", "bazel-out", "k8-fastbuild", "bin", "out.txt")
	_, err := os.Stat(localOutput)
	require.True(t, os.IsNotExist(err), "minimal build must retain only output metadata")
	c := f.Clients(harness.CacheProxy)
	upstream := f.Clients(harness.App)
	rng := rand.New(rand.NewSource(20261012))
	t.Log("eviction seed=20261012")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	payload := []byte("eviction-payload\n")
	digest := harness.Digest(payload)
	b, err := c.Read(ctx, "eviction", digest)
	require.NoError(t, err)
	require.Equal(t, payload, b)
	// Incompressible blobs exceed the private cache capacity. A bounded poll
	// proves eviction actually occurred before testing client recovery.
	evicted := false
	for round := 0; round < 10; round++ {
		for i := 0; i < 16; i++ {
			churn := make([]byte, 1<<20)
			_, err := rng.Read(churn)
			require.NoError(t, err)
			_, err = c.Upload(ctx, "eviction", churn)
			require.NoError(t, err)
			_, err = upstream.Upload(ctx, "eviction", churn)
			require.NoError(t, err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(2 * time.Second):
		}
		_, err = c.Read(ctx, "eviction", digest)
		if status.Code(err) == codes.NotFound {
			_, upstreamErr := upstream.Read(ctx, "eviction", digest)
			if status.Code(upstreamErr) == codes.NotFound {
				evicted = true
				break
			}
			require.NoError(t, upstreamErr)
		}
		require.NoError(t, err)
	}
	require.True(t, evicted, "test setup failed to evict the original output")
	args[4] = "//:later"
	run("minimal")
	localOutput = filepath.Join(filepath.Dir(localOutput), "later.txt")
	b, err = os.ReadFile(localOutput)
	require.NoError(t, err)
	require.Equal(t, payload, b, "Bazel must regenerate and download output after CAS eviction")
}

func workspaceFlags(version string) []string {
	if strings.HasPrefix(version, "7.") {
		return nil
	}
	return []string{"--enable_workspace", "--incompatible_autoload_externally="}
}
func asyncFlag(version string) string {
	if strings.HasPrefix(version, "7.") {
		return "--experimental_remote_cache_async"
	}
	return "--remote_cache_async"
}

// Independent batch clients race uploads of the same action and CAS blobs.
// After all upload barriers complete, fresh readers must see every output.
func TestConcurrentAsyncUploadsThroughProxy(t *testing.T) {
	bin := bazelClient(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	workspace := t.TempDir()
	write(t, filepath.Join(workspace, "WORKSPACE"), "workspace(name=\"async_stress\")\n")
	rng := rand.New(rand.NewSource(314159))
	t.Log("async upload seed=314159 clients=3 outputs=12")
	var build strings.Builder
	payloads := make([]string, 12)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("seeded-%016x-%d", rng.Uint64(), i)
		fmt.Fprintf(&build, "genrule(name=\"o%d\", outs=[\"o%d.txt\"], cmd=\"printf '%s' > $@\")\n", i, i, payloads[i])
	}
	write(t, filepath.Join(workspace, "BUILD"), build.String())
	run := func(t *testing.T, read bool) {
		base := t.TempDir()
		args := []string{"--batch", "--output_base=" + base, "--ignore_all_rc_files", "build", "//...", "--enable_bzlmod=false", "--jobs=2", "--remote_cache=grpc://" + f.Endpoint(harness.CacheProxy).GRPC, "--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--remote_instance_name=concurrent-async", "--remote_cache_async", "--remote_download_outputs=all", "--remote_timeout=60"}
		args = append(args, workspaceFlags("8.4.2")...)
		if read {
			args = append(args, "--remote_upload_local_results=false")
		} else {
			args = append(args, "--noremote_accept_cached")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir, cmd.Env = workspace, clientEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		t.Log(string(out))
		if read {
			require.Contains(t, string(out), "12 remote cache hit")
		}
		for i, payload := range payloads {
			b, err := os.ReadFile(filepath.Join(base, "execroot", "async_stress", "bazel-out", "k8-fastbuild", "bin", fmt.Sprintf("o%d.txt", i)))
			require.NoError(t, err)
			require.Equal(t, payload, string(b))
		}
	}
	t.Run("writers", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			t.Run(fmt.Sprint(i), func(t *testing.T) { t.Parallel(); run(t, false) })
		}
	})
	t.Run("reader", func(t *testing.T) { run(t, true) })
}
