package cache_stats_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var seed = flag.Int64("cache_stats.seed", 314159, "Deterministic workload seed")
var iterations = flag.Int("cache_stats.iterations", 1, "Number of independent three-build workloads (1..5)")

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any         { a, _ := v.([]any); return a }
func num(v any) int64 {
	switch x := v.(type) {
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case float64:
		return int64(x)
	}
	return 0
}

// Download a pinned released Bazel client; the client runs only inside this remote
// test. No BuildBuddy source is compiled. Batch mode prevents leaked JVM servers.
func bazelClient(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://releases.bazel.build/7.4.1/release/bazel-7.4.1-linux-x86_64", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	path := filepath.Join(t.TempDir(), "bazel")
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0755)
	require.NoError(t, err)
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, hash), io.LimitReader(resp.Body, 100<<20))
	require.NoError(t, err)
	require.NoError(t, out.Close())
	require.Equal(t, "c97f02133adce63f0c28678ac1f21d65fa8255c80429b588aeeba8a1fac6202b", fmt.Sprintf("%x", hash.Sum(nil)), "released Bazel checksum")
	return path
}

func TestSeededInvocationCacheStats(t *testing.T) {
	t.Logf("cache_stats.seed=%d iterations=%d", *seed, *iterations)
	require.True(t, *iterations >= 1 && *iterations <= 5, "iterations must be 1..5; seed=%d", *seed)
	rng := rand.New(rand.NewSource(*seed))
	client := bazelClient(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{harness.App: {"cache": harness.Config{"detailed_stats_enabled": true}}}})
	t.Logf("app image=%s executor image=%s", f.Endpoint(harness.App).Image, f.Endpoint(harness.Executor).Image)
	for iteration := 0; iteration < *iterations; iteration++ {
		t.Run(fmt.Sprintf("iteration-%d", iteration), func(t *testing.T) {
			t.Logf("replay seed=%d iteration=%d", *seed, iteration)
			work := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(work, "WORKSPACE"), []byte("workspace(name = \"cache_stats\")\n"), 0644))
			// Native genrules avoid dependency fetches. Different input sizes and
			// target counts exercise content deduplication.
			count := 4 + rng.Intn(5)
			var build strings.Builder
			payloads := make([][]byte, count)
			for i := 0; i < count; i++ {
				payloads[i] = make([]byte, 1+rng.Intn(8192))
				for j := range payloads[i] {
					payloads[i][j] = byte('a' + rng.Intn(26))
				}
				require.NoError(t, os.WriteFile(filepath.Join(work, fmt.Sprintf("in%d", i)), payloads[i], 0644))
				fmt.Fprintf(&build, "genrule(name='a%d', srcs=['in%d'], outs=['out%d'], cmd='cat $(SRCS) > $@; cat $(SRCS) >> $@')\n", i, i, i)
			}
			require.NoError(t, os.WriteFile(filepath.Join(work, "BUILD"), []byte(build.String()), 0644))
			changed := 1 + rng.Intn(count-1)
			for phase := 0; phase < 3; phase++ {
				if phase == 2 {
					for i := 0; i < changed; i++ {
						payloads[i] = append(payloads[i], byte('A'+i))
						require.NoError(t, os.WriteFile(filepath.Join(work, fmt.Sprintf("in%d", i)), payloads[i], 0644))
					}
				}
				id := uuid.NewString()
				probeModel := probes(t, f, id, rng)
				logPath := filepath.Join(work, fmt.Sprintf("spawn-%d.json", phase))
				root := t.TempDir()
				args := []string{"--batch", "--ignore_all_rc_files", "--output_user_root=" + root, "--host_jvm_args=-Xmx256m", "build", "//...", "--enable_bzlmod=false", "--jobs=3", "--remote_cache=grpc://" + f.Endpoint(harness.App).GRPC, "--bes_backend=grpc://" + f.Endpoint(harness.App).GRPC, "--remote_header=x-buildbuddy-api-key=" + f.APIKey, "--bes_header=x-buildbuddy-api-key=" + f.APIKey, "--invocation_id=" + id, "--execution_log_json_file=" + logPath, "--remote_download_outputs=all", "--remote_cache_compression=false", "--remote_upload_local_results=true", "--remote_timeout=60", "--bes_timeout=30s", "--strategy=Genrule=local", "--remote_default_exec_properties=OSFamily=linux", "--remote_default_exec_properties=Arch=amd64", "--remote_default_exec_properties=workload-isolation-type=none"}
				if phase == 2 {
					args = append(args, "--remote_executor=grpc://"+f.Endpoint(harness.App).GRPC, "--strategy=Genrule=remote")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				cmd := exec.CommandContext(ctx, client, args...)
				cmd.Dir = work
				// Strip outer-runner dispatch credentials and inherited Bazel settings.
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + work, "TMPDIR=" + os.TempDir()}
				output, err := cmd.CombinedOutput()
				cancel()
				require.NoError(t, err, "seed=%d iteration=%d phase=%d Bazel: %s", *seed, iteration, phase, output)
				t.Logf("phase=%d local invocation=%s Bazel: %s", phase, id, output)
				b, err := os.ReadFile(logPath)
				require.NoError(t, err)
				if dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); dir != "" {
					require.NoError(t, os.WriteFile(filepath.Join(dir, id+"-execution.json"), b, 0644))
				}
				decoder := json.NewDecoder(strings.NewReader(string(b)))
				hits, remote, total := int64(0), int64(0), int64(0)
				var spawns []map[string]any
				for {
					var spawn map[string]any
					err := decoder.Decode(&spawn)
					if err == io.EOF {
						break
					}
					require.NoError(t, err)
					if spawn["mnemonic"] != "Genrule" {
						continue
					}
					total++
					spawns = append(spawns, spawn)
					require.Zero(t, num(spawn["exitCode"]))
					if spawn["cacheHit"] == true {
						hits++
					} else if spawn["runner"] == "remote" {
						remote++
					}
				}
				require.Equal(t, int64(count), total, "seed=%d phase=%d log=%s", *seed, phase, b)
				expectedHits := int64(0)
				if phase == 1 {
					expectedHits = int64(count)
				}
				if phase == 2 {
					expectedHits = int64(count - changed)
				}
				require.Equal(t, expectedHits, hits, "seed=%d phase=%d", *seed, phase)
				if phase == 2 {
					require.Equal(t, int64(changed), remote)
				}
				inv, rows := readStats(t, f, id)
				if dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); dir != "" {
					data, err := json.MarshalIndent(map[string]any{"invocation": inv, "scorecard": rows, "probeModel": probeModel}, "", "  ")
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(filepath.Join(dir, id+"-stats.json"), data, 0644))
				}
				stats := obj(inv["cacheStats"])
				require.NotNil(t, stats)
				require.Equal(t, hits, num(stats["actionCacheHits"]), "seed=%d phase=%d aggregate AC hits", *seed, phase)
				// The released executor's default pre-execution GetActionResult check
				// adds one AC miss per executed action (executor.go, v2.314.0).
				require.Equal(t, total-hits+remote, num(stats["actionCacheMisses"]), "seed=%d phase=%d aggregate AC misses", *seed, phase)
				// The app caches both the ActionResult and an AC-backed ExecuteResponse
				// per successful remote execution (execution_server.go, v2.314.0).
				uploads := total - hits + remote
				require.Equal(t, uploads, num(stats["actionCacheUploads"]), "seed=%d phase=%d aggregate AC uploads", *seed, phase)
				model := scoreModel(t, rows)
				// Full stats include executor transfers, which the scorecard intentionally
				// excludes. Cache-only builds have an exact client-side accounting oracle.
				for key, want := range model {
					if phase < 2 {
						require.Equal(t, want, num(stats[key]), "seed=%d phase=%d field=%s stats=%v", *seed, phase, key, stats)
					} else {
						require.GreaterOrEqual(t, num(stats[key]), want, "seed=%d phase=%d field=%s", *seed, phase, key)
					}
				}
				acHits := int64(0)
				acMisses := int64(0)
				for _, row := range rows {
					if row["cacheType"] == "AC" && row["requestType"] == "READ" && row["actionMnemonic"] == "Genrule" {
						if num(obj(row["status"])["code"]) == 0 {
							acHits++
						} else {
							acMisses++
						}
					}
				}
				require.Equal(t, hits, acHits, "seed=%d phase=%d", *seed, phase)
				require.Equal(t, total-hits, acMisses, "seed=%d phase=%d", *seed, phase)
				verifyProbes(t, rows, probeModel)
				for _, spawn := range spawns {
					// Match each action to its request and each output to its CAS transfer,
					// using digests from Bazel rather than trusting API totals alone.
					matched := false
					for _, row := range rows {
						if row["cacheType"] == "AC" && row["requestType"] == "READ" && obj(row["digest"])["hash"] == obj(spawn["digest"])["hash"] {
							require.Equal(t, spawn["targetLabel"], row["targetId"])
							require.Equal(t, spawn["cacheHit"] == true, num(obj(row["status"])["code"]) == 0)
							matched = true
						}
					}
					require.True(t, matched, "seed=%d action %v absent from scorecard", *seed, spawn["targetLabel"])
					require.Len(t, list(spawn["actualOutputs"]), 1, "seed=%d output absent from execution log", *seed)
					for _, file := range list(spawn["actualOutputs"]) {
						d := obj(obj(file)["digest"])
						direction := "READ"
						if phase == 0 {
							direction = "WRITE"
						}
						matched = false
						for _, row := range rows {
							if row["cacheType"] == "CAS" && row["requestType"] == direction && obj(row["digest"])["hash"] == d["hash"] {
								require.Equal(t, num(d["sizeBytes"]), num(obj(row["digest"])["sizeBytes"]))
								require.Equal(t, num(d["sizeBytes"]), num(row["transferredSizeBytes"]))
								matched = true
							}
						}
						require.True(t, matched, "seed=%d output %v missing CAS %s", *seed, d, direction)
					}
				}

				response, err := f.Web.RPC("GetExecution", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "executionLookup": map[string]any{"invocationId": id}})
				require.NoError(t, err)
				require.Equal(t, remote, int64(len(list(response["execution"]))), "seed=%d phase=%d execution response=%v", *seed, phase, response)
				for i, payload := range payloads {
					out, err := os.ReadFile(filepath.Join(work, "bazel-bin", fmt.Sprintf("out%d", i)))
					require.NoError(t, err)
					require.Equal(t, append(append([]byte{}, payload...), payload...), out)
				}
			}
		})
	}
}

// Before BES finishes the invocation, issue uniquely labeled identity-compressed
// transfers with independently known sizes. These catch missing attribution,
// deduplication of repeated reads, and incorrect byte accounting.
func probes(t *testing.T, f *harness.Fixture, id string, rng *rand.Rand) map[string]int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: id, ActionId: "probe", ActionMnemonic: "StatsProbe", TargetId: "//probe:bytes"})
	require.NoError(t, err)
	ctx = metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(md))
	c := f.Clients(harness.App)
	want := map[string]int64{}
	// Include empty blobs and sizes around block boundaries. The one-byte case
	// deliberately exceeds the scorecard's 100-row page size with repeated reads.
	for _, size := range []int{0, 1, 4096, 65537, 1 + rng.Intn(16384)} {
		payload := make([]byte, size)
		_, err = rng.Read(payload)
		require.NoError(t, err)
		d, err := c.Upload(ctx, "stats-probes", payload)
		require.NoError(t, err)
		want["writes"]++
		want["uploadBytes"] += int64(size)
		reads := 1 + rng.Intn(4)
		if size == 1 {
			reads += 100
		}
		for i := 0; i < reads; i++ {
			b, err := c.Read(ctx, "stats-probes", d)
			require.NoError(t, err)
			require.True(t, bytes.Equal(payload, b), "seed=%d size=%d probe read mismatch", *seed, size)
		}
		want["hits"] += int64(reads)
		want["downloadBytes"] += int64(reads * size)
	}
	missing := harness.Digest([]byte("missing-" + id))
	_, err = c.Read(ctx, "stats-probes", missing)
	require.Equal(t, codes.NotFound, status.Code(err))
	want["misses"] = 1
	return want
}

func readStats(t *testing.T, f *harness.Fixture, id string) (map[string]any, []map[string]any) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var inv map[string]any
	for time.Now().Before(deadline) {
		r, err := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
		if err == nil && len(list(r["invocation"])) == 1 {
			inv = obj(list(r["invocation"])[0])
			if inv["invocationStatus"] == "COMPLETE_INVOCATION_STATUS" || num(inv["invocationStatus"]) == 1 {
				probe, scoreErr := f.Web.RPC("GetCacheScoreCard", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id})
				if scoreErr == nil && len(list(probe["results"])) > 0 && num(obj(inv["cacheStats"])["casCacheUploads"]) > 0 {
					break
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	require.NotNil(t, inv, "invocation %s missing", id)
	require.True(t, inv["invocationStatus"] == "COMPLETE_INVOCATION_STATUS" || num(inv["invocationStatus"]) == 1, "invocation not complete: %v", inv["invocationStatus"])
	var rows []map[string]any
	token := ""
	seen := map[string]bool{}
	for pages := 0; pages < 20; pages++ {
		r, err := f.Web.RPC("GetCacheScoreCard", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id, "pageToken": token, "orderBy": "ORDER_BY_START_TIME"})
		require.NoError(t, err)
		for _, v := range list(r["results"]) {
			rows = append(rows, obj(v))
		}
		token, _ = r["nextPageToken"].(string)
		if token == "" {
			return inv, rows
		}
		require.False(t, seen[token], "repeated scorecard page token")
		seen[token] = true
	}
	t.Fatal("scorecard exceeded bounded pagination")
	return nil, nil
}

func scoreModel(t *testing.T, rows []map[string]any) map[string]int64 {
	t.Helper()
	m := map[string]int64{"actionCacheHits": 0, "actionCacheMisses": 0, "actionCacheUploads": 0, "casCacheHits": 0, "casCacheMisses": 0, "casCacheUploads": 0, "totalUploadSizeBytes": 0, "totalDownloadSizeBytes": 0, "totalUploadTransferredSizeBytes": 0, "totalDownloadTransferredSizeBytes": 0}
	require.NotEmpty(t, rows)
	for _, r := range rows {
		prefix := "casCache"
		if r["cacheType"] == "AC" {
			prefix = "actionCache"
		}
		code := num(obj(r["status"])["code"])
		size := num(obj(r["digest"])["sizeBytes"])
		transferred := num(r["transferredSizeBytes"])
		require.GreaterOrEqual(t, size, int64(0))
		require.GreaterOrEqual(t, transferred, int64(0))
		if r["requestType"] == "WRITE" {
			require.Zero(t, code)
			m[prefix+"Uploads"]++
			m["totalUploadSizeBytes"] += size
			m["totalUploadTransferredSizeBytes"] += transferred
		} else if code == 0 {
			m[prefix+"Hits"]++
			m["totalDownloadSizeBytes"] += size
			m["totalDownloadTransferredSizeBytes"] += transferred
		} else {
			require.Equal(t, int64(5), code)
			m[prefix+"Misses"]++
		}
	}
	return m
}
func verifyProbes(t *testing.T, rows []map[string]any, want map[string]int64) {
	t.Helper()
	got := map[string]int64{}
	for _, r := range rows {
		if r["actionMnemonic"] != "StatsProbe" {
			continue
		}
		require.Equal(t, "CAS", r["cacheType"])
		size := num(obj(r["digest"])["sizeBytes"])
		if r["requestType"] == "WRITE" {
			got["writes"]++
			got["uploadBytes"] += size
			require.Equal(t, size, num(r["transferredSizeBytes"]))
		} else if num(obj(r["status"])["code"]) == 0 {
			got["hits"]++
			got["downloadBytes"] += size
			require.Equal(t, size, num(r["transferredSizeBytes"]))
		} else {
			got["misses"]++
		}
	}
	require.Equal(t, want, got, "seed=%d", *seed)
}
