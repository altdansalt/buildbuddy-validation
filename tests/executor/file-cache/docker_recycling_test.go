package filecache_test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/types/known/durationpb"
)

const actionImage = "mirror.gcr.io/library/alpine@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507"

func TestDockerRecycledAndFreshInputs(t *testing.T) {
	if *iterations < 1 || *iterations > 100 {
		t.Fatal("iterations must be 1..100")
	}
	for _, clone := range []bool{false, true} {
		t.Run(fmt.Sprintf("always-clone-%t", clone), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			// Bootstrap the fixture-owned daemon before mounting its socket into the executor.
			app := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
			host := dockerHost(t, ctx, app)
			socket := strings.TrimPrefix(host, "unix://")
			if socket == "" {
				socket = "/var/run/docker.sock"
			}
			t.Setenv("DOCKER_HOST", "unix://"+socket)
			root := t.TempDir()
			// Registered before the executor fixture: stop containers before removing root-owned files.
			t.Cleanup(func() {
				cleanupCtx, done := context.WithTimeout(context.Background(), 20*time.Second)
				defer done()
				b, err := exec.CommandContext(cleanupCtx, "sudo", "-n", "rm", "-rf", "--", root).CombinedOutput()
				if err != nil {
					t.Errorf("workspace cleanup: %v %s", err, b)
				}
			})
			executor := harness.Start(t, harness.Options{
				Components: []harness.Component{harness.Executor}, AppTarget: app.Endpoint(harness.App).InternalGRPC,
				Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{
					"docker_socket": "/var/run/docker.sock", "root_directory": filepath.Join(root, "remotebuilds"),
					"host_root_directory": root, "local_cache_directory": filepath.Join(root, "filecache"),
					"local_cache_size_bytes": cacheSize, "local_cache_always_clone": clone,
					"default_isolation_type": "docker", "default_image": actionImage, "docker_network": "none",
				}}},
				DockerArgs: map[harness.Component][]string{harness.Executor: {"--network", app.Network, "--volume", socket + ":/var/run/docker.sock", "--volume", root + ":" + root}},
			})
			c := app.Clients(harness.App)
			rng := rand.New(rand.NewSource(*seed))
			data := make([]byte, 128<<10)
			rng.Read(data)
			shared, err := c.Upload(ctx, instance, data)
			if err != nil {
				t.Fatal(err)
			}
			run := func(ds []*repb.Digest, recycle bool, id string) string {
				t.Helper()
				var script strings.Builder
				files := make([]*repb.FileNode, len(ds))
				for i, d := range ds {
					name := fmt.Sprintf("input%03d", i)
					files[i] = &repb.FileNode{Name: name, Digest: d, IsExecutable: i%2 == 1}
					fmt.Fprintf(&script, "test \"$(sha256sum %s | cut -d ' ' -f 1)\" = %s || exit 41; ", name, d.Hash)
				}
				// Removing/replacing an input path must not remove the cache's owned link.
				script.WriteString("rm input000; printf replacement > input000; hostname")
				rd, err := c.UploadProto(ctx, instance, &repb.Directory{Files: files})
				if err != nil {
					t.Fatal(err)
				}
				cd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", script.String(), id}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{
					{Name: "Arch", Value: "amd64"}, {Name: "OSFamily", Value: "linux"}, {Name: "container-image", Value: "docker://" + actionImage},
					{Name: "recycle-runner", Value: fmt.Sprint(recycle)}, {Name: "runner-recycling-test-key", Value: "filecache"}, {Name: "workload-isolation-type", Value: "docker"},
				}}})
				if err != nil {
					t.Fatal(err)
				}
				ad, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cd, InputRootDigest: rd, DoNotCache: true, Timeout: durationpb.New(time.Minute)})
				if err != nil {
					t.Fatal(err)
				}
				stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: ad, SkipCacheLookup: true})
				if err != nil {
					t.Fatal(err)
				}
				for {
					op, err := stream.Recv()
					if err == io.EOF {
						t.Fatal("no completed operation")
					}
					if err != nil {
						t.Fatal(err)
					}
					if !op.Done {
						continue
					}
					if op.GetError() != nil {
						t.Fatalf("seed=%d %s: %v", *seed, id, op.GetError())
					}
					res := &repb.ExecuteResponse{}
					if err := op.GetResponse().UnmarshalTo(res); err != nil {
						t.Fatal(err)
					}
					if res.GetStatus().GetCode() != 0 || res.Result == nil || res.Result.ExitCode != 0 {
						t.Fatalf("seed=%d %s: %v", *seed, id, res)
					}
					out := res.Result.StdoutRaw
					if res.Result.StdoutDigest != nil {
						out, err = c.Read(ctx, instance, res.Result.StdoutDigest)
						if err != nil {
							t.Fatal(err)
						}
					}
					return strings.TrimSpace(string(out))
				}
			}
			first := run([]*repb.Digest{shared, shared}, true, "warm")
			// Observe asynchronous pooling before requesting reuse, rather than assuming completion implies pooling.
			deadline := time.Now().Add(20 * time.Second)
			for {
				logs, err := dockerRaw(ctx, host, "logs", executor.Endpoint(harness.Executor).Container)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(logs), "Successfully recycled runner") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("runner was not pooled")
				}
				time.Sleep(100 * time.Millisecond)
			}
			cacheFiles, err := dockerRaw(ctx, host, "exec", executor.Endpoint(harness.Executor).Container, "find", filepath.Join(root, "filecache"), "-type", "f")
			if err != nil || !strings.Contains(string(cacheFiles), shared.Hash) {
				t.Fatalf("warm file-cache precondition: %v %s", err, cacheFiles)
			}
			for i := 0; i < *iterations; i++ {
				b := make([]byte, cacheSize+4096+rng.Intn(8192))
				rng.Read(b)
				d, err := c.Upload(ctx, instance, b)
				if err != nil {
					t.Fatal(err)
				}
				fresh := run([]*repb.Digest{shared, shared, d}, false, fmt.Sprintf("fresh-%d", i))
				if fresh == first {
					t.Fatal("fresh action reused pooled Docker container")
				}
				reused := run([]*repb.Digest{shared, shared}, true, fmt.Sprintf("recycled-%d", i))
				if reused != first {
					t.Fatalf("did not reuse Docker container: %q != %q", reused, first)
				}
			}
			t.Logf("seed=%d iterations=%d clone=%t recycled-container=%s", *seed, *iterations, clone, first)
		})
	}
}
