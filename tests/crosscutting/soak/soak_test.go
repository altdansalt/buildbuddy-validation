package soak_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

type sample struct {
	Minute    int
	Component harness.Component
	Values    map[string]float64
}
type latency struct {
	Minute   int
	Kind     string
	Duration time.Duration
}

// TestMixedLoadStability runs ten minutes of moderate load after setup. The
// corpus and action are bounded: growth cannot be attributed to new test keys.
func TestMixedLoadStability(t *testing.T) {
	components := []harness.Component{harness.App, harness.CacheProxy, harness.Executor}
	args := map[harness.Component][]string{}
	for _, c := range components {
		args[c] = []string{"-p", "127.0.0.1::9090"}
	}
	f := harness.Start(t, harness.Options{Components: components, DockerArgs: args})
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	const instance = "soak"
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	input := bytes.Repeat([]byte("bounded soak input\n"), 2048)
	d, err := proxy.Upload(ctx, instance, input)
	require.NoError(t, err)
	root, err := app.UploadProto(ctx, instance, &repb.Directory{Files: []*repb.FileNode{{Name: "input", Digest: d}}})
	require.NoError(t, err)
	cmd, err := app.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", "dd if=/dev/zero of=scratch bs=1048576 count=4 2>/dev/null && cat input > output"}, OutputPaths: []string{"output"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
	require.NoError(t, err)
	action, err := app.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second), DoNotCache: true})
	require.NoError(t, err)
	acKey := harness.Digest([]byte("bounded soak AC key"))
	expected := &repb.ActionResult{ExitCode: 0, StdoutRaw: []byte("soak AC")}
	_, err = proxy.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: acKey, ActionResult: expected})
	require.NoError(t, err)
	docker := dockerFor(t, f.Endpoint(harness.App).Container)
	endpoints := map[harness.Component]string{}
	for _, c := range components {
		b, e := docker("port", f.Endpoint(c).Container, "9090/tcp")
		require.NoError(t, e, string(b))
		endpoints[c] = "http://" + strings.TrimSpace(string(b)) + "/metrics"
		t.Logf("%s image=%s metrics=%s", c, f.Endpoint(c).Image, endpoints[c])
	}
	start := time.Now()
	var mu sync.Mutex
	var samples []sample
	var latencies []latency
	var failures []string
	record := func(kind string, begin time.Time, e error) {
		mu.Lock()
		defer mu.Unlock()
		latencies = append(latencies, latency{int(time.Since(start) / time.Minute), kind, time.Since(begin)})
		if e != nil {
			failures = append(failures, fmt.Sprintf("%s at %s: %v", kind, time.Since(start), e))
		}
	}
	var wg sync.WaitGroup
	loadCtx, stop := context.WithTimeout(ctx, 10*time.Minute)
	defer stop()
	worker := func(kind string, period time.Duration, work func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(period)
			defer ticker.Stop()
			for {
				select {
				case <-loadCtx.Done():
					return
				case <-ticker.C:
					begin := time.Now()
					rpc, done := context.WithTimeout(ctx, 45*time.Second)
					e := work(rpc)
					done()
					record(kind, begin, e)
				}
			}
		}()
	}
	worker("cache", time.Second, func(ctx context.Context) error {
		got, e := proxy.Upload(ctx, instance, input)
		if e != nil {
			return e
		}
		if got.Hash != d.Hash {
			return fmt.Errorf("upload digest mismatch")
		}
		for _, c := range []*harness.Clients{proxy, app} {
			b, e := c.Read(ctx, instance, d)
			if e != nil {
				return e
			}
			if !bytes.Equal(b, input) {
				return fmt.Errorf("corrupt CAS read")
			}
			r, e := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: acKey})
			if e != nil {
				return e
			}
			if r.ExitCode != 0 || !bytes.Equal(r.StdoutRaw, expected.StdoutRaw) {
				return fmt.Errorf("inconsistent AC result: %v", r)
			}
		}
		return nil
	})
	worker("execution", 5*time.Second, func(ctx context.Context) error {
		stream, e := app.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
		if e != nil {
			return e
		}
		for {
			op, e := stream.Recv()
			if e == io.EOF {
				return fmt.Errorf("execution ended without completion")
			}
			if e != nil {
				return e
			}
			if !op.Done {
				continue
			}
			if op.GetError() != nil {
				return fmt.Errorf("operation: %v", op.GetError())
			}
			r := &repb.ExecuteResponse{}
			if e := op.GetResponse().UnmarshalTo(r); e != nil {
				return e
			}
			if r.GetStatus().GetCode() != 0 || r.GetResult() == nil || r.Result.ExitCode != 0 {
				return fmt.Errorf("execution failed: %v", r)
			}
			if len(r.Result.OutputFiles) != 1 || r.Result.OutputFiles[0].Path != "output" {
				return fmt.Errorf("incorrect outputs: %v", r.Result)
			}
			b, e := proxy.Read(ctx, instance, r.Result.OutputFiles[0].Digest)
			if e != nil {
				return e
			}
			if !bytes.Equal(b, input) {
				return fmt.Errorf("corrupt execution output")
			}
			return nil
		}
	})
	client := &http.Client{Timeout: 10 * time.Second}
	for minute := 0; minute <= 10; minute++ {
		for _, c := range components {
			values, e := metrics(client, endpoints[c])
			require.NoError(t, e)
			disk, diskErr := docker("exec", f.Endpoint(c).Container, "du", "-sk", "/data")
			if diskErr == nil {
				fields := strings.Fields(string(disk))
				if len(fields) > 0 {
					if kb, err := strconv.ParseFloat(fields[0], 64); err == nil {
						values["data_directory_bytes"] = kb * 1024
					}
				}
			} else if minute == 0 {
				t.Logf("%s du unavailable: %v %s; disk metric coverage only", c, diskErr, disk)
			}
			samples = append(samples, sample{minute, c, values})
			t.Logf("minute=%d component=%s resources=%v", minute, c, values)
		}
		if minute < 10 {
			timer := time.NewTimer(time.Until(start.Add(time.Duration(minute+1) * time.Minute)))
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	stop()
	wg.Wait()
	artifact, _ := json.MarshalIndent(struct {
		Samples   []sample
		Latencies []latency
		Errors    []string
	}{samples, latencies, failures}, "", "  ")
	require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, "soak.json"), artifact, 0600))
	require.Empty(t, failures, "mixed load RPC/data errors")
	for _, c := range components {
		base, last := samples[2*len(components)+index(components, c)].Values, samples[10*len(components)+index(components, c)].Values
		for metric, allowance := range map[string]float64{"data_directory_bytes": 256 << 20, "go_goroutines": 150, "process_open_fds": 128, "go_memstats_heap_alloc_bytes": 256 << 20, "process_resident_memory_bytes": 384 << 20, "buildbuddy_remote_execution_runner_pool_disk_usage_bytes": 256 << 20, "buildbuddy_remote_cache_pebble_cache_pebble_level_size_bytes": 256 << 20} {
			if b, ok := base[metric]; ok {
				require.LessOrEqual(t, last[metric], b+allowance, "%s %s grew beyond conservative steady-state budget", c, metric)
			}
		}
		logs, e := docker("logs", f.Endpoint(c).Container)
		require.NoError(t, e)
		require.NotContains(t, string(logs), "panic:")
		require.NotContains(t, string(logs), "fatal error:")
	}
	for _, kind := range []string{"cache", "execution"} {
		var early, late []time.Duration
		for _, l := range latencies {
			if l.Kind != kind {
				continue
			}
			if l.Minute >= 1 && l.Minute < 3 {
				early = append(early, l.Duration)
			}
			if l.Minute >= 8 {
				late = append(late, l.Duration)
			}
		}
		require.GreaterOrEqual(t, len(early), 10)
		require.GreaterOrEqual(t, len(late), 10)
		a, b := median(early), median(late)
		t.Logf("%s median early=%s late=%s total_operations=%d", kind, a, b, len(latencies))
		require.LessOrEqual(t, b, 5*a+time.Second, "%s latency drift", kind)
	}
}
func index(cs []harness.Component, c harness.Component) int {
	for i, v := range cs {
		if v == c {
			return i
		}
	}
	panic("missing component")
}
func median(v []time.Duration) time.Duration {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[len(v)/2]
}
func metrics(client *http.Client, url string) (map[string]float64, error) {
	r, e := client.Get(url)
	if e != nil {
		return nil, e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, fmt.Errorf("metrics HTTP %d", r.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if e != nil {
		return nil, e
	}
	values := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.SplitN(fields[0], "{", 2)[0]
		if name == "go_goroutines" || name == "process_open_fds" || name == "process_resident_memory_bytes" || name == "go_memstats_heap_alloc_bytes" || name == "buildbuddy_remote_execution_runner_pool_disk_usage_bytes" || name == "buildbuddy_remote_cache_pebble_cache_pebble_level_size_bytes" {
			v, e := strconv.ParseFloat(fields[1], 64)
			if e == nil {
				values[name] += v
			}
		}
	}
	if _, ok := values["go_goroutines"]; !ok {
		return nil, fmt.Errorf("Go metrics absent")
	}
	return values, nil
}

// Find the daemon owning exactly our fixture container, including the harness's
// private-daemon fallback; never inspect unrelated containers or workloads.
func dockerFor(t *testing.T, container string) func(...string) ([]byte, error) {
	t.Helper()
	hosts := []string{""}
	sockets, _ := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	for _, s := range sockets {
		hosts = append(hosts, "unix://"+s)
	}
	for _, host := range hosts {
		run := func(args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if host != "" {
				args = append([]string{"--host", host}, args...)
			}
			return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		}
		if _, e := run("inspect", container); e == nil {
			return run
		}
	}
	t.Fatal("cannot locate fixture Docker daemon")
	return nil
}
