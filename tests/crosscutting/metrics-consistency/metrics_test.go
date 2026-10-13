package metrics_test

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

var seed = flag.Int64("metrics.seed", 314159, "Replay seed")
var iterations = flag.Int("metrics.iterations", 24, "Cache workload iterations (1..500)")
var sampleRE = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{.*\})? ([^ ]+)(?: [0-9]+)?$`)
var labelRE = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

type sample struct {
	name, key string
	labels    map[string]string
	value     float64
}
type snapshot struct {
	samples map[string]sample
	types   map[string]string
	schemas map[string]string
}

func scrape(ctx context.Context, url string) (snapshot, string, error) {
	s := snapshot{map[string]sample{}, map[string]string{}, map[string]string{}}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return s, "", err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return s, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return s, "", err
	}
	if resp.StatusCode != 200 {
		return s, string(b), fmt.Errorf("scrape HTTP %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(b)))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# TYPE ") {
			f := strings.Fields(line)
			if len(f) != 4 {
				return s, string(b), fmt.Errorf("bad TYPE %q", line)
			}
			s.types[f[2]] = f[3]
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleRE.FindStringSubmatch(line)
		if m == nil {
			return s, string(b), fmt.Errorf("invalid sample %q", line)
		}
		v, e := strconv.ParseFloat(m[3], 64)
		if e != nil || math.IsNaN(v) {
			return s, string(b), fmt.Errorf("invalid value %q", line)
		}
		labels := map[string]string{}
		var keys []string
		for _, l := range labelRE.FindAllStringSubmatch(m[2], -1) {
			val, e := strconv.Unquote(`"` + l[2] + `"`)
			if e != nil {
				return s, string(b), e
			}
			if _, ok := labels[l[1]]; ok {
				return s, string(b), fmt.Errorf("duplicate label %q", line)
			}
			labels[l[1]] = val
			keys = append(keys, l[1])
		}
		sort.Strings(keys)
		schema := strings.Join(keys, ",")
		if old, ok := s.schemas[m[1]]; ok && old != schema {
			return s, string(b), fmt.Errorf("inconsistent labels %s: %s vs %s", m[1], old, schema)
		}
		s.schemas[m[1]] = schema
		key := m[1] + m[2]
		if _, ok := s.samples[key]; ok {
			return s, string(b), fmt.Errorf("duplicate series %s", key)
		}
		s.samples[key] = sample{m[1], key, labels, v}
	}
	return s, string(b), scanner.Err()
}
func (s snapshot) total(name string, filter map[string]string) float64 {
	var n float64
	for _, x := range s.samples {
		if x.name != name {
			continue
		}
		match := true
		for k, v := range filter {
			if x.labels[k] != v {
				match = false
			}
		}
		if match {
			n += x.value
		}
	}
	return n
}
func checkSnapshots(a, b snapshot) error {
	for name, typ := range a.types {
		if bt, ok := b.types[name]; ok && bt != typ {
			return fmt.Errorf("type changed for %s", name)
		}
	}
	for name, schema := range a.schemas {
		if bs, ok := b.schemas[name]; ok && bs != schema {
			return fmt.Errorf("labels changed for %s", name)
		}
	}
	for key, x := range a.samples {
		typ := a.types[x.name]
		counter := typ == "counter" || strings.HasSuffix(x.name, "_count") || strings.HasSuffix(x.name, "_bucket")
		if y, ok := b.samples[key]; ok && counter && y.value < x.value {
			return fmt.Errorf("counter decreased %s: %g -> %g", key, x.value, y.value)
		}
	}
	// Every classic histogram's +Inf bucket equals its count, and finite buckets are cumulative.
	for _, x := range b.samples {
		if !strings.HasSuffix(x.name, "_count") {
			continue
		}
		base := strings.TrimSuffix(x.name, "_count")
		if b.types[base] != "histogram" {
			continue
		}
		type bucket struct{ bound, value float64 }
		var buckets []bucket
		for _, y := range b.samples {
			if y.name != base+"_bucket" {
				continue
			}
			match := true
			for k, v := range x.labels {
				if y.labels[k] != v {
					match = false
				}
			}
			if !match {
				continue
			}
			bound, e := strconv.ParseFloat(y.labels["le"], 64)
			if e != nil {
				return e
			}
			buckets = append(buckets, bucket{bound, y.value})
		}
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].bound < buckets[j].bound })
		if len(buckets) == 0 || !math.IsInf(buckets[len(buckets)-1].bound, 1) || buckets[len(buckets)-1].value != x.value {
			return fmt.Errorf("histogram count mismatch %s", x.key)
		}
		for i := 1; i < len(buckets); i++ {
			if buckets[i].value < buckets[i-1].value {
				return fmt.Errorf("noncumulative histogram %s", x.key)
			}
		}
	}
	return nil
}

func TestSeededMetricsConsistency(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("REPLAY: -metrics.seed=%d -metrics.iterations=%d", *seed, *iterations)
		}
	})
	require.True(t, *iterations > 0 && *iterations <= 500)
	components := []harness.Component{harness.App, harness.Executor, harness.CacheProxy}
	args := map[harness.Component][]string{}
	urls := map[harness.Component]string{}
	for _, c := range components {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, e)
		port := l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())
		args[c] = []string{"-p", fmt.Sprintf("127.0.0.1:%d:9090", port)}
		urls[c] = fmt.Sprintf("http://127.0.0.1:%d/metrics", port)
	}
	f := harness.Start(t, harness.Options{Components: components, DockerArgs: args})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	before := map[harness.Component]snapshot{}
	for _, c := range components {
		s, raw, e := scrape(ctx, urls[c])
		require.NoError(t, e)
		require.NotEmpty(t, s.samples)
		before[c] = s
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, string(c)+"-metrics-before.txt"), []byte(raw), 0644))
	}
	// Scrape each component continuously while RPCs mutate its metrics.
	loadCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, c := range components {
		wg.Add(1)
		go func(c harness.Component) {
			defer wg.Done()
			previous := before[c]
			for {
				select {
				case <-loadCtx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				s, _, e := scrape(loadCtx, urls[c])
				if loadCtx.Err() != nil {
					return
				}
				if e == nil {
					e = checkSnapshots(previous, s)
				}
				if e != nil {
					errs <- fmt.Errorf("%s: %w", c, e)
					return
				}
				previous = s
			}
		}(c)
	}
	defer func() { stop(); wg.Wait() }()
	rng := rand.New(rand.NewSource(*seed))
	proxy := f.Clients(harness.CacheProxy)
	app := f.Clients(harness.App)
	var uploadBytes, readBytes, reads, misses float64
	for i := 0; i < *iterations; i++ {
		size := []int{1, 2, 9, 10, 11, 999, 1000, 1001, 65536}[rng.Intn(9)]
		data := make([]byte, size)
		_, e := rng.Read(data)
		require.NoError(t, e)
		// Unique instance and nonempty random payload avoid empty-CAS and deduplication shortcuts.
		instance := fmt.Sprintf("metrics/%d/%d", *seed, i)
		missing := harness.Digest([]byte(fmt.Sprintf("missing-%d-%d", *seed, i)))
		_, e = proxy.Read(ctx, instance, missing)
		require.Equal(t, codes.NotFound, status.Code(e), "iteration=%d", i)
		reads++
		misses++
		d, e := proxy.Write(ctx, instance, data, false)
		require.NoError(t, e, "iteration=%d", i)
		uploadBytes += float64(size)
		hitCount := 1 + rng.Intn(4)
		for j := 0; j < hitCount; j++ {
			got, e := proxy.Read(ctx, instance, d)
			require.NoError(t, e, "iteration=%d read=%d", i, j)
			require.Equal(t, data, got)
			readBytes += float64(size)
			reads++
		}
	}
	// Measure app transfer histograms separately: execution uploads/downloads are excluded.
	mid, _, e := scrape(ctx, urls[harness.App])
	require.NoError(t, e)
	var directBytes float64
	for i := 0; i < 8; i++ {
		data := make([]byte, 1+rng.Intn(8192))
		rng.Read(data)
		d, e := app.Upload(ctx, "direct", data)
		require.NoError(t, e)
		got, e := app.Read(ctx, "direct", d)
		require.NoError(t, e)
		require.Equal(t, data, got)
		directBytes += float64(len(data))
	}
	afterDirect, _, e := scrape(ctx, urls[harness.App])
	require.NoError(t, e)
	for _, kind := range []string{"upload", "download"} {
		name := "buildbuddy_remote_cache_" + kind + "_size_bytes"
		filter := map[string]string{"cache_type": "cas"}
		require.Equal(t, "cache_type,group_id,server_name,tracked", afterDirect.schemas[name+"_count"], name)
		require.Equal(t, float64(8), afterDirect.total(name+"_count", filter)-mid.total(name+"_count", filter), kind)
		require.Equal(t, directBytes, afterDirect.total(name+"_sum", filter)-mid.total(name+"_sum", filter), kind)
	}
	exits := map[string]float64{}
	var executionUsec float64
	for i := 0; i < 6; i++ {
		exit := []int{0, 1, 7}[i%3]
		if i >= 3 {
			exit = []int{0, 1, 7}[rng.Intn(3)]
		}
		exits[strconv.Itoa(exit)]++
		executionUsec += runAction(t, ctx, app, i, exit)
	}
	// RPC completion can precede the executor's deferred metrics update.
	var final map[harness.Component]snapshot
	require.Eventually(t, func() bool {
		final = map[harness.Component]snapshot{}
		for _, c := range components {
			s, _, err := scrape(ctx, urls[c])
			if err != nil {
				return false
			}
			final[c] = s
		}
		return final[harness.Executor].total("buildbuddy_remote_execution_count", nil)-before[harness.Executor].total("buildbuddy_remote_execution_count", nil) == 6
	}, 10*time.Second, 100*time.Millisecond)
	stop()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "seed=%d", *seed)
	}
	for _, c := range components {
		require.NoError(t, checkSnapshots(before[c], final[c]))
		_, raw, e := scrape(ctx, urls[c])
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, string(c)+"-metrics-after.txt"), []byte(raw), 0644))
	}
	p := final[harness.CacheProxy]
	b := before[harness.CacheProxy]
	for name, want := range map[string]float64{"byte_stream_read_requests": reads, "byte_stream_write_requests": float64(*iterations), "byte_stream_read_bytes": readBytes, "byte_stream_write_bytes": uploadBytes} {
		full := "buildbuddy_proxy_" + name
		require.Equal(t, want, p.total(full, nil)-b.total(full, nil), full)
	}
	require.Equal(t, misses, p.total("buildbuddy_proxy_byte_stream_read_requests", map[string]string{"status": "NotFound"})-b.total("buildbuddy_proxy_byte_stream_read_requests", map[string]string{"status": "NotFound"}))
	require.Equal(t, "cache_status,chunked,compression,proxy_request_type,status", p.schemas["buildbuddy_proxy_byte_stream_read_requests"])
	require.Equal(t, "exit_code,isolation,status", final[harness.Executor].schemas["buildbuddy_remote_execution_count"])
	durationName := "buildbuddy_remote_execution_executed_action_metadata_durations_usec"
	stage := map[string]string{"stage": "execution"}
	require.Equal(t, "group_id,stage", final[harness.Executor].schemas[durationName+"_count"])
	require.Equal(t, float64(6), final[harness.Executor].total(durationName+"_count", stage)-before[harness.Executor].total(durationName+"_count", stage))
	require.Equal(t, executionUsec, final[harness.Executor].total(durationName+"_sum", stage)-before[harness.Executor].total(durationName+"_sum", stage))
	for exit, n := range exits {
		filter := map[string]string{"exit_code": exit}
		require.Equal(t, n, final[harness.Executor].total("buildbuddy_remote_execution_count", filter)-before[harness.Executor].total("buildbuddy_remote_execution_count", filter), "exit=%s", exit)
	}
}
func runAction(t *testing.T, ctx context.Context, c *harness.Clients, i, exit int) float64 {
	t.Helper()
	root, e := c.UploadProto(ctx, "exec", &repb.Directory{})
	require.NoError(t, e)
	cmd, e := c.UploadProto(ctx, "exec", &repb.Command{Arguments: []string{"/bin/sh", "-c", fmt.Sprintf("printf '%d'; exit %d", i, exit)}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
	require.NoError(t, e)
	action, e := c.UploadProto(ctx, "exec", &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(20 * time.Second), DoNotCache: true})
	require.NoError(t, e)
	stream, e := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "exec", ActionDigest: action, SkipCacheLookup: true})
	require.NoError(t, e)
	for {
		op, e := stream.Recv()
		require.NoError(t, e)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		result := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(result))
		require.Zero(t, result.GetStatus().GetCode())
		require.EqualValues(t, exit, result.GetResult().GetExitCode())
		md := result.GetResult().GetExecutionMetadata()
		require.NotNil(t, md.GetExecutionStartTimestamp())
		require.NotNil(t, md.GetExecutionCompletedTimestamp())
		return float64(md.GetExecutionCompletedTimestamp().AsTime().Sub(md.GetExecutionStartTimestamp().AsTime()).Microseconds())
	}
}
