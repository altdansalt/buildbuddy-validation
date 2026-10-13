package filecache_test

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/types/known/durationpb"
)

var seed = flag.Int64("filecache.seed", 314159, "Deterministic file-cache workload seed")
var iterations = flag.Int("filecache.iterations", 12, "Random actions per phase (1..100)")

const cacheSize = 1 << 20
const instance = "file-cache"

// The reference model is the SHA256 digest of every uploaded byte string.
// REAPI FileNode identifies content by digest; execution must see that content.
// v2.314.0 filecache.go documents that eviction unlinks only cache-owned links.
func TestFileCacheProperties(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	if *iterations < 1 || *iterations > 100 {
		t.Fatal("iterations must be 1..100")
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"local_cache_size_bytes": cacheSize}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	docker := fixtureDocker(t, ctx, f)
	r := rand.New(rand.NewSource(*seed))
	upload := func(n int) *repb.Digest {
		b := make([]byte, n)
		if _, err := r.Read(b); err != nil {
			t.Fatal(err)
		}
		d, err := c.Upload(ctx, instance, b)
		if err != nil {
			t.Fatalf("seed=%d upload: %v", *seed, err)
		}
		return d
	}
	shared := upload(128 << 10)
	// Each command hashes all inputs both before and after the hold point.
	run := func(ds []*repb.Digest, id string, held bool) error {
		files := make([]*repb.FileNode, len(ds))
		var checks strings.Builder
		for i, d := range ds {
			name := fmt.Sprintf("input%03d", i)
			files[i] = &repb.FileNode{Name: name, Digest: d, IsExecutable: i%2 == 1}
			fmt.Fprintf(&checks, "test \"$(sha256sum %s | cut -d ' ' -f 1)\" = %s || exit 41; ", name, d.Hash)
		}
		root, err := c.UploadProto(ctx, instance, &repb.Directory{Files: files})
		if err != nil {
			return err
		}
		script := checks.String()
		if held {
			script += "touch /tmp/filecache-held; while test ! -e /tmp/filecache-release; do sleep 0.1; done; "
		}
		script += checks.String() + "printf verified"
		cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", script, id}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}, {Name: "estimated-compute-units", Value: "1"}}}})
		if err != nil {
			return err
		}
		action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, DoNotCache: true, Timeout: durationpb.New(90 * time.Second)})
		if err != nil {
			return err
		}
		stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
		if err != nil {
			return err
		}
		for {
			op, err := stream.Recv()
			if err == io.EOF {
				return fmt.Errorf("no completed operation")
			}
			if err != nil {
				return err
			}
			if !op.Done {
				continue
			}
			if op.GetError() != nil {
				return fmt.Errorf("operation: %v", op.GetError())
			}
			response := &repb.ExecuteResponse{}
			if err := op.GetResponse().UnmarshalTo(response); err != nil {
				return err
			}
			if response.GetStatus().GetCode() != 0 {
				return fmt.Errorf("execution: %v", response.Status)
			}
			result := response.GetResult()
			if result == nil || result.ExitCode != 0 {
				return fmt.Errorf("result: %v", result)
			}
			out := result.StdoutRaw
			if result.StdoutDigest != nil {
				out, err = c.Read(ctx, instance, result.StdoutDigest)
				if err != nil {
					return err
				}
			}
			if string(out) != "verified" {
				return fmt.Errorf("stdout=%q", out)
			}
			return nil
		}
	}
	check := func(ds []*repb.Digest, id string) {
		t.Helper()
		if err := run(ds, id, false); err != nil {
			t.Fatalf("seed=%d case=%s sizes=%v: %v", *seed, id, sizes(ds), err)
		}
	}
	check([]*repb.Digest{shared}, "warm")
	// Verify this is a populated cache, rather than merely repeated CAS downloads.
	cacheFiles := func() string {
		return docker("exec", f.Endpoint(harness.Executor).Container, "sh", "-c", "find /data/filecache -type f")
	}
	if !strings.Contains(cacheFiles(), shared.Hash) {
		t.Fatal("shared digest absent from warmed file cache")
	}
	heldDone := make(chan error, 1)
	go func() { heldDone <- run([]*repb.Digest{shared}, "held", true) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := dockerRaw(ctx, dockerHost(t, ctx, f), "exec", f.Endpoint(harness.Executor).Container, "test", "-e", "/tmp/filecache-held")
		_ = out
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("seed=%d held action did not start", *seed)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Unique random data above aggregate capacity forces eviction while held input
	// is live. Sizes are around filesystem block boundaries and the cache limit.
	boundaries := []int{0, 1, 4095, 4096, 4097, cacheSize - 4096, cacheSize, cacheSize + 4096, 2 * cacheSize}
	for i, n := range boundaries {
		check([]*repb.Digest{upload(n)}, fmt.Sprintf("boundary-%d", i))
	}
	if strings.Contains(cacheFiles(), shared.Hash) {
		t.Fatalf("seed=%d workload did not evict shared input", *seed)
	}
	docker("exec", f.Endpoint(harness.Executor).Container, "touch", "/tmp/filecache-release")
	if err := <-heldDone; err != nil {
		t.Fatalf("seed=%d input changed during eviction: %v", *seed, err)
	}
	// Generate once, then replay identical inputs after a warm restart.
	workload := make([][]*repb.Digest, *iterations)
	for i := range workload {
		workload[i] = []*repb.Digest{shared, shared, upload(1 + r.Intn(cacheSize+8192))}
	}
	batch := func(phase string) {
		var wg sync.WaitGroup
		errs := make(chan string, len(workload))
		slots := make(chan struct{}, 4)
		for i, ds := range workload {
			wg.Add(1)
			go func(i int, ds []*repb.Digest) {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				id := fmt.Sprintf("%s-%d", phase, i)
				if err := run(ds, id, false); err != nil {
					errs <- fmt.Sprintf("seed=%d case=%s sizes=%v: %v", *seed, id, sizes(ds), err)
				}
			}(i, ds)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
		if t.Failed() {
			t.FailNow()
		}
	}
	batch("concurrent")
	check([]*repb.Digest{shared}, "rewarm")
	if !strings.Contains(cacheFiles(), shared.Hash) {
		t.Fatal("warm restart precondition missing")
	}
	docker("restart", f.Endpoint(harness.Executor).Container)
	if !strings.Contains(cacheFiles(), shared.Hash) {
		t.Fatal("restart lost cache volume")
	}
	batch("restart")
}
func sizes(ds []*repb.Digest) []int64 {
	ns := make([]int64, len(ds))
	for i, d := range ds {
		ns[i] = d.SizeBytes
	}
	return ns
}
func dockerRaw(ctx context.Context, host string, args ...string) ([]byte, error) {
	if host != "" {
		args = append([]string{"--host", host}, args...)
	}
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

// The harness currently keeps its owned Docker socket private. Discover it by
// inspecting ONLY this fixture's unique container name, never another fixture.
func dockerHost(t *testing.T, ctx context.Context, f *harness.Fixture) string {
	t.Helper()
	hosts := []string{""}
	sockets, _ := filepath.Glob(filepath.Join("/tmp", "bbval-docker-*", "docker.sock"))
	for _, s := range sockets {
		hosts = append(hosts, "unix://"+s)
	}
	for _, h := range hosts {
		if _, err := dockerRaw(ctx, h, "inspect", f.Endpoint(harness.Executor).Container); err == nil {
			return h
		}
	}
	t.Fatal("fixture Docker socket not found")
	return ""
}
func fixtureDocker(t *testing.T, ctx context.Context, f *harness.Fixture) func(...string) string {
	host := dockerHost(t, ctx, f)
	return func(args ...string) string {
		t.Helper()
		b, err := dockerRaw(ctx, host, args...)
		if err != nil {
			t.Fatalf("fixture docker %v: %v %s", args, err, b)
		}
		return string(b)
	}
}
