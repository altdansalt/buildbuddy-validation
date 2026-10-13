package metrics_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
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
	bspb "google.golang.org/genproto/googleapis/bytestream"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTransfersInvocationsAndRestart(t *testing.T) {
	t.Logf("seed=%d", *seed)
	components := []harness.Component{harness.App, harness.CacheProxy, harness.Executor}
	args := map[harness.Component][]string{}
	urls := map[harness.Component]string{}
	for _, c := range components {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())
		args[c] = []string{"-p", fmt.Sprintf("127.0.0.1:%d:9090", port)}
		urls[c] = fmt.Sprintf("http://127.0.0.1:%d/metrics", port)
	}
	f := harness.Start(t, harness.Options{Components: components, DockerArgs: args})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	rng := rand.New(rand.NewSource(*seed))
	take := func(c harness.Component, label string) snapshot {
		s, raw, err := scrape(ctx, urls[c])
		require.NoError(t, err)
		require.NoError(t, checkSnapshots(s, s))
		for _, x := range s.samples {
			if strings.HasPrefix(x.name, "buildbuddy_") && (s.types[x.name] == "counter" || strings.HasSuffix(x.name, "_bucket") || strings.HasSuffix(x.name, "_count") || strings.Contains(x.name, "bytes_per_second")) {
				require.GreaterOrEqual(t, x.value, float64(0), x.key)
			}
		}
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, string(c)+"-"+label+".txt"), []byte(raw), 0644))
		return s
	}
	previousEpoch := map[harness.Component]snapshot{}
	for epoch := 0; epoch < 2; epoch++ {
		t.Logf("epoch=%d", epoch)
		before := map[harness.Component]snapshot{}
		for _, c := range components {
			before[c] = take(c, fmt.Sprintf("epoch%d-before", epoch))
			if epoch > 0 {
				schemaOnly := previousEpoch[c]
				schemaOnly.samples = nil
				require.NoError(t, checkSnapshots(schemaOnly, before[c]))
			}
		}
		// Model actual wire bytes independently of digest bytes. Include compressible
		// and incompressible inputs; range offsets/limits refer to identity data.
		for _, c := range []harness.Component{harness.App, harness.CacheProxy} {
			client := app
			if c == harness.CacheProxy {
				client = proxy
			}
			b := take(c, fmt.Sprintf("epoch%d-transfers-before", epoch))
			var uploads, downloads float64
			for i := 0; i < 8; i++ {
				data := make([]byte, 1024+rng.Intn(32000))
				_, err := rng.Read(data)
				require.NoError(t, err)
				if i%2 == 0 {
					data = bytes.Repeat([]byte{byte(epoch*8 + i)}, len(data))
				}
				instance := fmt.Sprintf("round2/%d/%s/%d", epoch, c, i)
				d, err := client.Write(ctx, instance, data, true)
				require.NoError(t, err)
				compressed, err := harness.Compress(data)
				require.NoError(t, err)
				uploads += float64(len(compressed))
				for _, compressedRead := range []bool{true, false} {
					offset, limit := int64(0), int64(0)
					want := data
					if !compressedRead {
						offset = int64(rng.Intn(len(data) - 1))
						limit = int64(1 + rng.Intn(len(data)-int(offset)))
						want = data[offset : offset+limit]
					}
					stream, err := client.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, compressedRead), ReadOffset: offset, ReadLimit: limit})
					require.NoError(t, err)
					var wire []byte
					for {
						resp, err := stream.Recv()
						if err == io.EOF {
							break
						}
						require.NoError(t, err)
						wire = append(wire, resp.Data...)
					}
					downloads += float64(len(wire))
					got := wire
					if compressedRead {
						got, err = harness.Decompress(wire)
						require.NoError(t, err)
					}
					require.Equal(t, want, got)
				}
			}
			a := take(c, fmt.Sprintf("epoch%d-transfers-after", epoch))
			require.NoError(t, checkSnapshots(b, a))
			if c == harness.CacheProxy {
				for name, want := range map[string]float64{"write_bytes": uploads, "read_bytes": downloads, "write_requests": 8, "read_requests": 16} {
					metric := "buildbuddy_proxy_byte_stream_" + name
					require.Equal(t, want, a.total(metric, nil)-b.total(metric, nil), metric)
				}
			} else {
				for kind, want := range map[string]float64{"upload": uploads, "download": downloads} {
					metric := "buildbuddy_server_" + kind + "_size_bytes"
					require.Equal(t, want, a.total(metric+"_sum", nil)-b.total(metric+"_sum", nil), metric)
					count := float64(8)
					if kind == "download" {
						count = 16
					}
					require.Equal(t, count, a.total(metric+"_count", nil)-b.total(metric+"_count", nil), metric)
				}
			}
		}
		// Invocation accounting is asynchronous, so poll after each closed BES stream.
		invBefore := take(harness.App, fmt.Sprintf("epoch%d-invocations-before", epoch))
		for _, exit := range []int{0, 3, 0} {
			publishMetricInvocation(t, ctx, f, app, exit)
		}
		require.Eventually(t, func() bool {
			s, _, err := scrape(ctx, urls[harness.App])
			return err == nil && s.total("buildbuddy_invocation_count", nil)-invBefore.total("buildbuddy_invocation_count", nil) == 3
		}, 20*time.Second, 100*time.Millisecond)
		invAfter := take(harness.App, fmt.Sprintf("epoch%d-invocations-after", epoch))
		for status, want := range map[string]float64{"success": 2, "failure": 1} {
			filter := map[string]string{"invocation_status": status}
			require.Equal(t, want, invAfter.total("buildbuddy_invocation_count", filter)-invBefore.total("buildbuddy_invocation_count", filter))
			require.Equal(t, want, invAfter.total("buildbuddy_invocation_duration_usec_count", filter)-invBefore.total("buildbuddy_invocation_duration_usec_count", filter))
		}
		runAction(t, ctx, app, 100+epoch, 0)
		require.Eventually(t, func() bool {
			s, _, err := scrape(ctx, urls[harness.Executor])
			return err == nil && s.total("buildbuddy_remote_execution_count", nil)-before[harness.Executor].total("buildbuddy_remote_execution_count", nil) == 1
		}, 15*time.Second, 100*time.Millisecond)
		for _, c := range components {
			a := take(c, fmt.Sprintf("epoch%d-after", epoch))
			require.NoError(t, checkSnapshots(before[c], a))
			previousEpoch[c] = a
		}
		if epoch == 0 {
			for _, c := range components {
				restartMetricContainer(t, ctx, f.Endpoint(c))
				require.Eventually(t, func() bool { s, _, err := scrape(ctx, urls[c]); return err == nil && len(s.samples) > 0 }, 30*time.Second, 200*time.Millisecond)
				// Dynamic workload series must reset; no old process counters may leak.
				s := take(c, "restart")
				metric := map[harness.Component]string{harness.App: "buildbuddy_invocation_count", harness.CacheProxy: "buildbuddy_proxy_byte_stream_read_requests", harness.Executor: "buildbuddy_remote_execution_count"}[c]
				require.Zero(t, s.total(metric, nil), metric)
			}
			app, proxy = f.Clients(harness.App), f.Clients(harness.CacheProxy)
			f.Web = harness.LoginSelfAuth(t, f.Endpoint(harness.App))
		}
	}
}

// Select only the daemon that owns this fixture's exact container name. This
// also supports the harness-owned socket without changing shared harness code.
func restartMetricContainer(t *testing.T, ctx context.Context, ep *harness.Endpoint) {
	name := ep.Container
	t.Helper()
	sockets, err := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	require.NoError(t, err)
	hosts := []string{""}
	for _, s := range sockets {
		hosts = append(hosts, "unix://"+s)
	}
	for _, host := range hosts {
		args := []string{}
		if host != "" {
			args = append(args, "--host", host)
		}
		probe := exec.CommandContext(ctx, "docker", append(args, "inspect", name)...)
		if probe.Run() != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, "docker", append(args, "restart", "--time", "10", name)...).CombinedOutput()
		require.NoError(t, err, string(out))
		// Docker may allocate new ephemeral published ports on restart.
		out, err = exec.CommandContext(ctx, "docker", append(args, "inspect", "--format", "{{json .NetworkSettings.Ports}}", name)...).Output()
		require.NoError(t, err)
		var ports map[string][]struct{ HostPort string }
		require.NoError(t, json.Unmarshal(out, &ports))
		require.NotEmpty(t, ports["8080/tcp"])
		ep.HTTP = "http://127.0.0.1:" + ports["8080/tcp"][0].HostPort
		if p := ports["1985/tcp"]; len(p) > 0 {
			ep.GRPC = "127.0.0.1:" + p[0].HostPort
		}
		require.Eventually(t, func() bool {
			req, err := http.NewRequestWithContext(ctx, "GET", ep.HTTP+"/readyz?server-type="+ep.ServerType, nil)
			if err != nil {
				return false
			}
			resp, err := (&http.Client{Timeout: time.Second}).Do(req)
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}, 30*time.Second, 200*time.Millisecond, "restart readiness %s", name)
		return
	}
	t.Fatalf("no Docker daemon owns fixture container %s", name)
}

func publishMetricInvocation(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, exit int) {
	t.Helper()
	field := func(n protowire.Number, b []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
	}
	num := func(n protowire.Number, v uint64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
	}
	iid := uuid.NewString()
	// Attach the invocation ID to cache RPCs. Cache statistics are flushed to the
	// invocation DB later than stream finalization and are read through the UI API.
	md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: iid})
	require.NoError(t, err)
	transferCtx := metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(md))
	data := bytes.Repeat([]byte(iid), 256)
	encoded, err := harness.Compress(data)
	require.NoError(t, err)
	var transferredDownload int64
	finishedID := field(9, nil)
	started := append(field(1, field(3, nil)), field(2, finishedID)...)
	payload := append(field(1, []byte(iid)), num(2, 1700000000000)...)
	payload = append(payload, field(3, []byte("7.0.0"))...)
	payload = append(payload, field(4, []byte("--color=no"))...)
	payload = append(payload, field(5, []byte("test"))...)
	started = append(started, field(5, payload)...)
	name := "SUCCESS"
	if exit != 0 {
		name = "BUILD_FAILURE"
	}
	finishPayload := append(field(3, append(field(1, []byte(name)), num(2, uint64(exit))...)), num(2, 1700000001000)...)
	finished := append(field(1, finishedID), field(14, finishPayload)...)
	finished = append(finished, num(20, 1)...)
	stream, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	id := &bes.StreamId{BuildId: iid, InvocationId: iid, Component: bes.StreamId_TOOL}
	for i, wire := range [][]byte{started, finished} {
		require.NoError(t, stream.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: id, SequenceNumber: int64(i + 1), Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: wire}}}}}))
		if i == 0 {
			d, err := c.Write(transferCtx, iid, data, true)
			require.NoError(t, err)
			for _, compressed := range []bool{true, false} {
				offset, limit := int64(0), int64(0)
				want := data
				if !compressed {
					offset = 17
					limit = 113
					want = data[offset : offset+limit]
				}
				read, err := c.ByteStream.Read(transferCtx, &bspb.ReadRequest{ResourceName: harness.ResourceName(iid, d, compressed), ReadOffset: offset, ReadLimit: limit})
				require.NoError(t, err)
				var wire []byte
				for {
					r, err := read.Recv()
					if err == io.EOF {
						break
					}
					require.NoError(t, err)
					wire = append(wire, r.Data...)
				}
				transferredDownload += int64(len(wire))
				got := wire
				if compressed {
					got, err = harness.Decompress(wire)
					require.NoError(t, err)
				}
				require.Equal(t, want, got)
			}
		}
	}
	require.NoError(t, stream.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: id, SequenceNumber: 3, Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_ComponentStreamFinished{ComponentStreamFinished: &bes.BuildEvent_BuildComponentStreamFinished{Type: bes.BuildEvent_BuildComponentStreamFinished_FINISHED}}}}}))
	require.NoError(t, stream.CloseSend())
	for seq := int64(1); seq <= 3; seq++ {
		ack, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, seq, ack.SequenceNumber)
	}
	_, err = stream.Recv()
	require.Equal(t, io.EOF, err)
	var stats map[string]any
	var lastErr error
	integer := func(name string) int64 {
		switch v := stats[name].(type) {
		case string:
			n, e := strconv.ParseInt(v, 10, 64)
			require.NoError(t, e, name)
			return n
		case float64:
			return int64(v)
		case nil:
			return 0
		default:
			t.Fatalf("unexpected %s: %T", name, v)
			return 0
		}
	}
	require.Eventually(t, func() bool {
		response, e := f.Web.RPC("GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": iid}, "requestContext": f.Web.RequestContext(f.GroupID)})
		lastErr = e
		if e != nil {
			return false
		}
		invs, _ := response["invocation"].([]any)
		if len(invs) != 1 {
			return false
		}
		inv, _ := invs[0].(map[string]any)
		stats, _ = inv["cacheStats"].(map[string]any)
		return integer("casCacheUploads") == 1 && integer("casCacheHits") == 2
	}, 20*time.Second, 100*time.Millisecond, "cache stats iid=%s err=%v", iid, lastErr)
	require.Equal(t, int64(len(data)), integer("totalUploadSizeBytes"))
	require.Equal(t, int64(2*len(data)), integer("totalDownloadSizeBytes"))
	require.Equal(t, int64(len(encoded)), integer("totalUploadTransferredSizeBytes"))
	require.Equal(t, transferredDownload, integer("totalDownloadTransferredSizeBytes"))
	for _, direction := range []string{"upload", "download"} {
		title := strings.ToUpper(direction[:1]) + direction[1:]
		usec := integer("total" + title + "Usec")
		rate := integer(direction + "ThroughputBytesPerSecond")
		require.GreaterOrEqual(t, usec, int64(0))
		require.GreaterOrEqual(t, rate, int64(0))
		want := int64(0)
		if usec > 0 {
			want = int64(float64(integer("total"+title+"SizeBytes")) / (float64(usec) / 1e6))
		}
		require.InDelta(t, want, rate, 1, direction+" throughput")
	}
	require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, iid+"-cache-stats.txt"), []byte(fmt.Sprint(stats)), 0644))
}
