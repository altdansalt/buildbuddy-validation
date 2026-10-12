package downloadmodes_test

import (
	"context"
	"crypto/sha256"
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
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func write(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0600))
}

// Pin and verify the standalone client; it runs only on the hosted test runner.
func bazelClient(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://releases.bazel.build/7.4.1/release/bazel-7.4.1-linux-x86_64", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	require.NoError(t, err)
	require.Equal(t, "c97f02133adce63f0c28678ac1f21d65fa8255c80429b588aeeba8a1fac6202b", fmt.Sprintf("%x", sha256.Sum256(b)))
	path := filepath.Join(t.TempDir(), "bazel")
	require.NoError(t, os.WriteFile(path, b, 0700))
	return path
}

func TestBazelDownloadModes(t *testing.T) {
	bin := bazelClient(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
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
				args := []string{"--batch", "--output_base=" + base, "--ignore_all_rc_files", "build", "//:final", "--enable_bzlmod=false", "--jobs=2", "--remote_cache=grpc://" + f.Endpoint(harness.App).GRPC, "--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--remote_download_outputs=" + mode, "--remote_cache_async", "--remote_instance_name=" + fmt.Sprintf("%s-%t-%t", mode, scenario.remote, disk), "--remote_timeout=60", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64", "--remote_default_exec_properties=workload-isolation-type=none"}
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
					// Do not expose the dispatch credential to the nested client.
					for _, e := range os.Environ() {
						if !strings.HasPrefix(e, "BUILDBUDDY_API_KEY=") {
							cmd.Env = append(cmd.Env, e)
						}
					}
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
				command, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf output"}})
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
				require.Equal(t, result.OutputFiles, restored.OutputFiles)
				require.Equal(t, result.OutputDirectories, restored.OutputDirectories)
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
		require.Equal(t, output, response.Result.OutputFiles[0].Digest)
		b, err := c.Read(ctx, instance, output)
		require.NoError(t, err)
		require.Equal(t, payload, b)
	}
}
