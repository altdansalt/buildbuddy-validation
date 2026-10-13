package tracking_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These wire fields follow the released protos; no BuildBuddy source is linked.
func join(p ...[]byte) []byte { return bytes.Join(p, nil) }
func b(n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, 2), v)
}
func s(n protowire.Number, v string) []byte { return b(n, []byte(v)) }
func v(n protowire.Number, x int64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, 0), uint64(x))
}

type codec struct{}

func (codec) Name() string                    { return "proto" }
func (codec) Marshal(x any) ([]byte, error)   { return x.([]byte), nil }
func (codec) Unmarshal(b []byte, x any) error { *(x.(*[]byte)) = append([]byte(nil), b...); return nil }
func fields(t *testing.T, wire []byte) map[protowire.Number][][]byte {
	t.Helper()
	out := map[protowire.Number][][]byte{}
	for len(wire) > 0 {
		n, typ, k := protowire.ConsumeTag(wire)
		require.Positive(t, k)
		wire = wire[k:]
		var val []byte
		if typ == 2 {
			val, k = protowire.ConsumeBytes(wire)
		} else {
			var x uint64
			x, k = protowire.ConsumeVarint(wire)
			val = []byte(strconv.FormatUint(x, 10))
		}
		require.Positive(t, k)
		out[n] = append(out[n], val)
		wire = wire[k:]
	}
	return out
}
func one(m map[protowire.Number][][]byte, n protowire.Number) []byte {
	if len(m[n]) == 0 {
		return nil
	}
	return m[n][0]
}
func number(x any) int64 {
	// Proto JSON and wire encoding omit default zero fields.
	if x == nil || x == "" {
		return 0
	}
	n, err := strconv.ParseInt(fmt.Sprint(x), 10, 64)
	if err != nil {
		panic(fmt.Sprintf("invalid numeric projection %v: %v", x, err))
	}
	return n
}
func call(t *testing.T, c *harness.Clients, method string, req []byte) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out []byte
	require.NoError(t, c.Conn.Invoke(ctx, method, req, &out, grpc.ForceCodec(codec{})))
	return out
}
func web(t *testing.T, f *harness.Fixture, method string, req map[string]any) map[string]any {
	t.Helper()
	req["requestContext"] = f.Web.RequestContext(f.GroupID)
	out, err := f.Web.RPC(method, req)
	require.NoError(t, err)
	return out
}
func invocation(t *testing.T, c *harness.Clients, id string, work func()) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stream, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	send := func(seq int64, payload []byte) {
		require.NoError(t, stream.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: &bes.StreamId{BuildId: id, InvocationId: id, Component: bes.StreamId_TOOL}, SequenceNumber: seq, Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: payload}}}}}))
	}
	send(1, join(b(1, b(3, nil)), b(5, join(s(1, id), v(2, time.Now().UnixMilli()), s(3, "7.4.1"), s(4, "--color=no"), s(5, "build")))))
	work()
	send(2, join(b(1, b(9, nil)), b(14, join(v(2, time.Now().UnixMilli()), b(3, join(s(1, "SUCCESS"), v(2, 0))))), v(20, 1)))
	require.NoError(t, stream.CloseSend())
	for i := 0; i < 2; i++ {
		_, err := stream.Recv()
		require.NoError(t, err)
	}
	_, err = stream.Recv()
	require.Equal(t, io.EOF, err)
}

type row struct {
	hash                                          string
	size, transferred, cache, request, compressor int64
}

func TestTrackingStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"cache": harness.Config{"detailed_stats_enabled": true}, "app": harness.Config{"usage_tracking_enabled": true, "region": "fixture"}}, harness.CacheProxy: {"cache_proxy": harness.Config{"remote_hit_tracker": harness.Config{"target": "grpc://app:1985"}}}}})
	t.Logf("app=%s proxy=%s", f.Endpoint(harness.App).Image, f.Endpoint(harness.CacheProxy).Image)
	app := f.Clients(harness.App)
	proxy := f.Clients(harness.CacheProxy)
	var acHits, casHits, downloads, uploads, customerDownloads int64
	historyModel := map[string]map[protowire.Number]int64{}
	for phase := 0; phase < 9; phase++ {
		id := uuid.NewString()
		var model []row
		invocation(t, app, id, func() {
			if phase < 8 {
				ct := int64(1 + phase/4)
				rt := int64(1 + (phase/2)%2)
				comp := int64(phase % 2)
				payload := bytes.Repeat([]byte("tracked-payload"), 20+phase)
				d := harness.Digest(payload)
				wire, err := proto.Marshal(d)
				require.NoError(t, err)
				transferred := d.SizeBytes
				if comp == 1 {
					z, err := harness.Compress(payload)
					require.NoError(t, err)
					transferred = int64(len(z))
				}
				md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: id, ActionId: "action", ActionMnemonic: "Compile", TargetId: "//fixture:target"})
				require.NoError(t, err)
				hit := join(b(1, md), b(2, join(b(1, wire), v(3, comp), v(4, ct))), v(3, transferred), b(4, v(2, 1000000)), v(5, rt))
				req := join(b(1, hit), s(2, "fixture-proxy"))
				// Track has no operation ID: identical valid requests represent two transfers.
				for repeat := 0; repeat < 2; repeat++ {
					call(t, app, "/hit_tracker.HitTrackerService/Track", req)
					model = append(model, row{d.Hash, d.SizeBytes, transferred, ct, rt, comp})
					liveCounters(t, f, id, model)
					if rt == 1 {
						downloads += d.SizeBytes
						if ct == 1 {
							acHits++
						} else {
							casHits++
						}
					} else {
						uploads += d.SizeBytes
					}
				}
				call(t, app, "/hit_tracker.HitTrackerService/Track", nil) // empty batch is a no-op.
				liveCounters(t, f, id, model)
			} else {
				// Observer scorecard/usage calls do not perform cache transfers. The workload
				// uses invocation metadata only here, and reads through both data surfaces.
				payload := bytes.Repeat([]byte("normal-transfer"), 100)
				d := harness.Digest(payload)
				md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: id, ActionId: "action", ActionMnemonic: "Compile", TargetId: "//fixture:target"})
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				ctx = metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(md))
				_, err = app.Upload(ctx, "", payload)
				require.NoError(t, err)
				uploads += d.SizeBytes
				model = append(model, row{d.Hash, d.SizeBytes, d.SizeBytes, 2, 2, 0})
				liveCounters(t, f, id, model)
				for readIndex, c := range []*harness.Clients{app, proxy, proxy} {
					got, err := c.Read(ctx, "", d)
					require.NoError(t, err)
					require.Equal(t, payload, got)
					// Cold proxy reads transfer bytes from the app; only the warm
					// locally served hit belongs to the customer-proxy bucket.
					if readIndex == 2 {
						customerDownloads += d.SizeBytes
					} else {
						downloads += d.SizeBytes
					}
					casHits++
					model = append(model, row{d.Hash, d.SizeBytes, d.SizeBytes, 2, 1, 0})
					liveCounters(t, f, id, model)
				}

				require.Eventually(t, func() bool {
					out, err := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
					if err != nil {
						return false
					}
					invs, _ := out["invocation"].([]any)
					if len(invs) == 0 {
						return false
					}
					stats, _ := invs[0].(map[string]any)["cacheStats"].(map[string]any)
					return number(stats["casCacheHits"]) == 3
				}, 30*time.Second, 250*time.Millisecond)
			}
		})
		req := join(b(1, s(2, f.GroupID)), s(2, id))
		require.Eventually(t, func() bool {
			out, err := f.Web.RPC("GetCacheScoreCard", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id})
			if err != nil {
				t.Logf("scorecard phase=%d: %v", phase, err)
			}
			return err == nil && out != nil
		}, 30*time.Second, 250*time.Millisecond)
		grpcRows := fields(t, call(t, app, "/buildbuddy.service.BuildBuddyService/GetCacheScoreCard", req))[2]
		httpRows, _ := web(t, f, "GetCacheScoreCard", map[string]any{"invocationId": id})["results"].([]any)
		require.Len(t, grpcRows, len(model))
		require.Len(t, httpRows, len(model))
		want := map[row]int{}
		for _, r := range model {
			want[r]++
		}
		actual := map[row]int{}
		httpActual := map[row]int{}
		for _, wire := range grpcRows {
			m := fields(t, wire)
			d := fields(t, one(m, 7))
			r := row{string(one(d, 1)), number(string(one(d, 2))), number(string(one(m, 11))), number(string(one(m, 14))), number(string(one(m, 5))), number(string(one(m, 10)))}
			actual[r]++
			require.Zero(t, number(string(one(fields(t, one(m, 6)), 1))), "status")
			if phase < 8 {
				require.Equal(t, int64(1000000), number(string(one(fields(t, one(m, 9)), 2))), "supplied duration nanos")
			}
			require.Equal(t, "Compile", string(one(m, 1)))
			require.Equal(t, "//fixture:target", string(one(m, 2)))
			require.Equal(t, "action", string(one(m, 3)))
		}
		for _, raw := range httpRows {
			m := raw.(map[string]any)
			d := m["digest"].(map[string]any)
			ct := enum(t, m["cacheType"], map[string]int64{"AC": 1, "CAS": 2})
			rt := enum(t, m["requestType"], map[string]int64{"READ": 1, "WRITE": 2})
			comp := enum(t, m["compressor"], map[string]int64{"IDENTITY": 0, "ZSTD": 1, "<nil>": 0})
			httpActual[row{fmt.Sprint(d["hash"]), number(d["sizeBytes"]), number(m["transferredSizeBytes"]), ct, rt, comp}]++
			require.Equal(t, "Compile", m["actionMnemonic"])
			require.Equal(t, "//fixture:target", m["targetId"])
			require.Equal(t, "action", m["actionId"])
			status, _ := m["status"].(map[string]any)
			require.Zero(t, number(status["code"]))
			if phase < 8 {
				require.Equal(t, "0.001s", m["duration"])
			}
		}
		require.Equal(t, want, actual)
		require.Equal(t, want, httpActual)
		for _, search := range []string{"Compile", "no-matching-record"} {
			filtered := web(t, f, "GetCacheScoreCard", map[string]any{"invocationId": id, "filter": map[string]any{"mask": "search", "search": search}, "orderBy": "ORDER_BY_DURATION", "groupBy": "GROUP_BY_ACTION"})
			rows, _ := filtered["results"].([]any)
			expected := 0
			if search == "Compile" {
				expected = len(model)
			}
			require.Len(t, rows, expected)
			filteredWire := fields(t, call(t, app, "/buildbuddy.service.BuildBuddyService/GetCacheScoreCard", join(req, b(4, join(b(1, s(1, "search")), s(5, search))), v(5, 2), v(8, 1))))
			require.Len(t, filteredWire[2], expected)
		}
		// Stable finalized scorecard, independently observed after a settling interval.
		time.Sleep(250 * time.Millisecond)
		require.Equal(t, grpcRows, fields(t, call(t, app, "/buildbuddy.service.BuildBuddyService/GetCacheScoreCard", req))[2])

		// GetInvocation backs the UI summary cards and persists aggregate stats.
		statsWant := map[protowire.Number]int64{}
		for _, r := range model {
			if r.request == 1 {
				statsWant[7] += r.size
				statsWant[14] += r.transferred
				if r.cache == 1 {
					statsWant[1]++
				} else {
					statsWant[4]++
				}
			} else {
				statsWant[8] += r.size
				statsWant[15] += r.transferred
				if r.cache == 1 {
					statsWant[3]++
				} else {
					statsWant[6]++
				}
			}
		}
		invReq := join(b(1, s(2, f.GroupID)), b(2, s(1, id)))
		inv := one(fields(t, call(t, app, "/buildbuddy.service.BuildBuddyService/GetInvocation", invReq)), 2)
		stats := fields(t, one(fields(t, inv), 18))
		httpInv := web(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})["invocation"].([]any)[0].(map[string]any)
		httpStats := httpInv["cacheStats"].(map[string]any)
		for n, name := range map[protowire.Number]string{1: "actionCacheHits", 2: "actionCacheMisses", 5: "casCacheMisses", 11: "totalCachedActionExecUsec", 16: "totalUncachedActionExecUsec", 3: "actionCacheUploads", 4: "casCacheHits", 6: "casCacheUploads", 7: "totalDownloadSizeBytes", 8: "totalUploadSizeBytes", 14: "totalDownloadTransferredSizeBytes", 15: "totalUploadTransferredSizeBytes"} {
			require.Equal(t, statsWant[n], number(string(one(stats, n))), name)
			require.Equal(t, statsWant[n], number(httpStats[name]), name)
		}

		if phase < 8 {
			timerField, throughputField := protowire.Number(9), protowire.Number(12)
			sizeField := protowire.Number(7)
			if model[0].request == 2 {
				timerField = 10
				throughputField = 13
				sizeField = 8
			}
			require.Equal(t, int64(len(model)*1000), number(string(one(stats, timerField))), "supplied aggregate duration")
			require.Equal(t, statsWant[sizeField]*1000/int64(len(model)), number(string(one(stats, throughputField))), "logical throughput")
			names := map[protowire.Number]string{9: "totalDownloadUsec", 10: "totalUploadUsec", 12: "downloadThroughputBytesPerSecond", 13: "uploadThroughputBytesPerSecond"}
			require.Equal(t, int64(len(model)*1000), number(httpStats[names[timerField]]))
			require.Equal(t, statsWant[sizeField]*1000/int64(len(model)), number(httpStats[names[throughputField]]))
		}

		// History exposes persisted cacheStats on its compact Invocation rows.
		historyModel[id] = statsWant
		for projectionSample := 0; projectionSample < 2; projectionSample++ {
			for _, transport := range []string{"http", "grpc"} {
				token := ""
				seen := map[string]bool{}
				for page := 0; page <= phase+1; page++ {
					var next string
					if transport == "http" {
						out := web(t, f, "SearchInvocation", map[string]any{"query": map[string]any{"groupId": f.GroupID}, "count": 1, "pageToken": token})
						rows, _ := out["invocation"].([]any)
						for _, raw := range rows {
							iid := raw.(map[string]any)["invocationId"].(string)
							require.False(t, seen[iid])
							seen[iid] = true
							hs := raw.(map[string]any)["cacheStats"].(map[string]any)
							for n, name := range map[protowire.Number]string{1: "actionCacheHits", 2: "actionCacheMisses", 3: "actionCacheUploads", 4: "casCacheHits", 5: "casCacheMisses", 6: "casCacheUploads", 7: "totalDownloadSizeBytes", 8: "totalUploadSizeBytes", 14: "totalDownloadTransferredSizeBytes", 15: "totalUploadTransferredSizeBytes"} {
								require.Equal(t, historyModel[iid][n], number(hs[name]), "history %s %s", iid, name)
							}
						}
						next, _ = out["nextPageToken"].(string)
					} else {
						out := fields(t, call(t, app, "/buildbuddy.service.BuildBuddyService/SearchInvocation", join(b(1, s(2, f.GroupID)), b(2, s(3, f.GroupID)), v(4, 1), s(5, token))))
						for _, raw := range out[2] {
							iid := string(one(fields(t, raw), 1))
							require.False(t, seen[iid])
							seen[iid] = true
							hs := fields(t, one(fields(t, raw), 18))
							for _, n := range []protowire.Number{1, 2, 3, 4, 5, 6, 7, 8, 14, 15} {
								require.Equal(t, historyModel[iid][n], number(string(one(hs, n))), "history %s field %d", iid, n)
							}
						}
						next = string(one(out, 3))
					}
					if next == "" {
						break
					}
					require.NotEqual(t, token, next)
					token = next
				}
				require.Len(t, seen, phase+1)
				require.True(t, seen[id])
			}
			trend := web(t, f, "GetTrend", map[string]any{"query": map[string]any{}})
			rows, _ := trend["trendStat"].([]any)
			totals := map[string]int64{}
			for _, raw := range rows {
				for _, name := range []string{"actionCacheHits", "casCacheHits", "totalDownloadSizeBytes", "totalUploadSizeBytes", "totalNumBuilds"} {
					totals[name] += number(raw.(map[string]any)[name])
				}
			}
			require.Equal(t, acHits, totals["actionCacheHits"])
			require.Equal(t, casHits, totals["casCacheHits"])
			require.Equal(t, downloads+customerDownloads, totals["totalDownloadSizeBytes"])
			require.Equal(t, uploads, totals["totalUploadSizeBytes"])
			require.Equal(t, int64(phase+1), totals["totalNumBuilds"])
			grpcTrend := fields(t, call(t, app, "/buildbuddy.service.BuildBuddyService/GetTrend", join(b(1, s(2, f.GroupID)), b(2, nil))))
			for n, name := range map[protowire.Number]string{10: "actionCacheHits", 13: "casCacheHits", 16: "totalDownloadSizeBytes", 17: "totalUploadSizeBytes", 3: "totalNumBuilds"} {
				var total int64
				for _, raw := range grpcTrend[2] {
					total += number(string(one(fields(t, raw), n)))
				}
				require.Equal(t, totals[name], total, name)
			}
			httpStat := web(t, f, "GetInvocationStat", map[string]any{"aggregationType": "GROUP_ID_AGGREGATION_TYPE", "query": map[string]any{}})
			statRows := httpStat["invocationStat"].([]any)
			require.Len(t, statRows, 1)
			require.Equal(t, int64(phase+1), number(statRows[0].(map[string]any)["totalNumBuilds"]))
			grpcStat := fields(t, call(t, app, "/buildbuddy.service.BuildBuddyService/GetInvocationStat", join(b(1, s(2, f.GroupID)), v(2, 3), b(4, nil))))
			require.Len(t, grpcStat[2], 1)
			require.Equal(t, int64(phase+1), number(string(one(fields(t, grpcStat[2][0]), 4))))
			time.Sleep(250 * time.Millisecond)
		}
	}
	var lastUsage string
	// Fixed one-minute usage bucket + ten-second settling + next minute flush.
	require.Eventually(t, func() bool {
		out, err := f.Web.RPC("GetUsage", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		if err != nil {
			t.Log(err)
			return false
		}
		u, _ := out["usage"].(map[string]any)
		snapshot := fmt.Sprint(u)
		if snapshot != lastUsage {
			t.Logf("usage=%s expected ac=%d cas=%d download=%d upload=%d customer_download=%d", snapshot, acHits, casHits, downloads, uploads, customerDownloads)
			lastUsage = snapshot
		}
		return number(u["actionCacheHits"]) == acHits && number(u["casCacheHits"]) == casHits && number(u["totalDownloadSizeBytes"]) == downloads && number(u["totalUploadSizeBytes"]) == uploads && number(u["totalCustomerProxyDownloadSizeBytes"]) == customerDownloads
	}, 150*time.Second, time.Second)
	// Usage reads require an org-admin principal, unlike cache operations.
	adminKey := f.Web.CreateAPIKey(t, f.GroupID, "usage-observer", []string{"ORG_ADMIN"})
	admin := harness.Dial(t, f.Endpoint(harness.App).GRPC, adminKey)
	u := fields(t, one(fields(t, call(t, admin, "/buildbuddy.service.BuildBuddyService/GetUsage", b(1, s(2, f.GroupID)))), 2))
	for n, want := range map[protowire.Number]int64{3: acHits, 4: casHits, 5: downloads, 7: uploads, 20: customerDownloads} {
		require.Equal(t, want, number(string(one(u, n))))
	}
	for sample := 0; sample < 2; sample++ {
		usageBuckets(t, f, admin)
		time.Sleep(time.Second)
		usage := web(t, f, "GetUsage", map[string]any{})["usage"].(map[string]any)
		for name, want := range map[string]int64{"actionCacheHits": acHits, "casCacheHits": casHits, "totalDownloadSizeBytes": downloads, "totalUploadSizeBytes": uploads, "totalCustomerProxyDownloadSizeBytes": customerDownloads} {
			require.Equal(t, want, number(usage[name]), name)
		}
	}
}

// Audit probe: real ByteStream writes, independent CAS and UI reads.
func TestTransferSurfaces(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	key := f.Web.CreateAPIKey(t, f.GroupID, "second-cache-client", []string{"CACHE_WRITE"})
	second := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key)
	cli := releasedCLI(t, ctx)
	t.Logf("app=%s proxy=%s", f.Endpoint(harness.App).Image, f.Endpoint(harness.CacheProxy).Image)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		for _, compressed := range []bool{false, true} {
			payload := bytes.Repeat([]byte(fmt.Sprintf("audit-%s-%t-", component, compressed)), 100)
			expectedDigest := harness.Digest(payload)
			for _, reader := range []harness.Component{harness.App, harness.CacheProxy} {
				missing, err := f.Clients(reader).CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{expectedDigest}, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				require.Len(t, missing.MissingBlobDigests, 1)
				require.True(t, proto.Equal(expectedDigest, missing.MissingBlobDigests[0]))
			}
			started := time.Now()
			d, err := f.Clients(component).Write(ctx, "", payload, compressed)
			require.NoError(t, err)
			// A repeated valid write must preserve all immutable CAS projections.
			for step := 0; step < 3; step++ {
				if step == 1 {
					repeated, err := second.Write(ctx, "", payload, !compressed)
					require.NoError(t, err)
					require.True(t, proto.Equal(d, repeated))
				}
				if step == 2 {
					repeated, err := f.Clients(harness.App).Upload(ctx, "", payload)
					require.NoError(t, err)
					require.True(t, proto.Equal(d, repeated))
				}
				for sample := 0; sample < 2; sample++ {
					for _, reader := range []harness.Component{harness.App, harness.CacheProxy} {
						cmd := exec.CommandContext(ctx, cli, "download", fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes), "--target=grpc://"+f.Endpoint(reader).GRPC, "--api_key="+key)
						cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
						var stderr bytes.Buffer
						cmd.Stderr = &stderr
						got, err := cmd.Output()
						require.NoError(t, err, stderr.String())
						require.Equal(t, payload, got, "bb download %s", reader)
					}
					for _, reader := range []harness.Component{harness.App, harness.CacheProxy} {
						c := f.Clients(reader)
						missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
						require.NoError(t, err)
						require.Empty(t, missing.MissingBlobDigests)
						batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
						require.NoError(t, err)
						require.Len(t, batch.Responses, 1)
						require.Zero(t, batch.Responses[0].GetStatus().GetCode())
						require.Equal(t, payload, batch.Responses[0].Data)
						require.True(t, proto.Equal(d, batch.Responses[0].Digest))
						require.Equal(t, repb.Compressor_IDENTITY, batch.Responses[0].Compressor)
						got, err := c.Read(ctx, "", d)
						require.NoError(t, err)
						require.Equal(t, payload, got)
					}
					wire, err := proto.Marshal(d)
					require.NoError(t, err)
					resource := join(b(1, wire), v(4, 2), v(5, 1))
					meta := fields(t, call(t, f.Clients(harness.App), "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", b(2, resource)))
					require.Equal(t, d.SizeBytes, number(string(one(meta, 5))))
					httpMeta := web(t, f, "GetCacheMetadata", map[string]any{"resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "cacheType": "CAS", "digestFunction": "SHA256"}})
					require.Equal(t, d.SizeBytes, number(httpMeta["digestSizeBytes"]))
					require.Positive(t, number(string(one(meta, 2))), "stored size")
					require.Equal(t, number(string(one(meta, 2))), number(httpMeta["storedSizeBytes"]))
					require.Positive(t, number(string(one(meta, 4))), "modification timestamp")
					require.Equal(t, number(string(one(meta, 4))), number(httpMeta["lastModifyUsec"]))
					bsURL := "bytestream://app:1985/" + harness.ResourceName("", d, false)
					publicFileSurfaces(t, ctx, f, bsURL, payload)
					for _, route := range []string{"download", "view"} {
						reqURL := f.Web.URL + "/file/" + route + "?bytestream_url=" + url.QueryEscape(bsURL) + "&request_context=" + url.QueryEscape(base64.StdEncoding.EncodeToString(s(2, f.GroupID)))
						resp, err := f.Web.Client.Get(reqURL)
						require.NoError(t, err)
						body, err := io.ReadAll(resp.Body)
						resp.Body.Close()
						require.NoError(t, err)
						require.Equal(t, 200, resp.StatusCode, string(body))
						require.Equal(t, payload, body)
					}
					if sample == 0 {
						t.Logf("writer=%s compressed=%t step=%d convergence<=%s", component, compressed, step, time.Since(started))
						time.Sleep(250 * time.Millisecond)
					}
				}
			}
		}
	}
}

// Live projections are the surfaces supported while a BES stream remains open.
func liveCounters(t *testing.T, f *harness.Fixture, id string, model []row) {
	t.Helper()
	wants := map[string]int64{}
	for _, r := range model {
		if r.request == 1 {
			wants["totalDownloadSizeBytes"] += r.size
			wants["totalDownloadTransferredSizeBytes"] += r.transferred
			if r.cache == 1 {
				wants["actionCacheHits"]++
			} else {
				wants["casCacheHits"]++
			}
		} else {
			wants["totalUploadSizeBytes"] += r.size
			wants["totalUploadTransferredSizeBytes"] += r.transferred
			if r.cache == 1 {
				wants["actionCacheUploads"]++
			} else {
				wants["casCacheUploads"]++
			}
		}
	}
	started := time.Now()
	read := func() bool {
		out, err := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
		if err != nil {
			return false
		}
		invs, _ := out["invocation"].([]any)
		if len(invs) != 1 {
			return false
		}
		stats, _ := invs[0].(map[string]any)["cacheStats"].(map[string]any)
		for name, want := range wants {
			if number(stats[name]) != want {
				return false
			}
		}
		return true
	}
	require.Eventually(t, read, 30*time.Second, 100*time.Millisecond)
	t.Logf("live invocation %s records=%d convergence<=%s", id, len(model), time.Since(started))
	for sample := 0; sample < 2; sample++ {
		require.True(t, read(), "converged HTTP stats must stay stable")
		inv := one(fields(t, call(t, f.Clients(harness.App), "/buildbuddy.service.BuildBuddyService/GetInvocation", join(b(1, s(2, f.GroupID)), b(2, s(1, id))))), 2)
		stats := fields(t, one(fields(t, inv), 18))
		for n, name := range map[protowire.Number]string{1: "actionCacheHits", 2: "actionCacheMisses", 5: "casCacheMisses", 11: "totalCachedActionExecUsec", 16: "totalUncachedActionExecUsec", 3: "actionCacheUploads", 4: "casCacheHits", 6: "casCacheUploads", 7: "totalDownloadSizeBytes", 8: "totalUploadSizeBytes", 14: "totalDownloadTransferredSizeBytes", 15: "totalUploadTransferredSizeBytes"} {
			require.Equal(t, wants[name], number(string(one(stats, n))), name)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func releasedCLI(t *testing.T, ctx context.Context) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	require.NoError(t, err)
	require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(data)))
	path := filepath.Join(t.TempDir(), "bb")
	require.NoError(t, os.WriteFile(path, data, 0755))
	return path
}

func TestScorecardPagination(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"cache": harness.Config{"detailed_stats_enabled": true}}}})
	app := f.Clients(harness.App)
	id := uuid.NewString()
	invocation(t, app, id, func() {
		var batch []byte
		for i := 0; i < 101; i++ {
			payload := bytes.Repeat([]byte("p"), i+1)
			digest, err := proto.Marshal(harness.Digest(payload))
			require.NoError(t, err)
			md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: id, ActionId: fmt.Sprint(i), ActionMnemonic: "Compile", TargetId: "//fixture:target"})
			require.NoError(t, err)
			batch = append(batch, b(1, join(b(1, md), b(2, join(b(1, digest), v(4, 2))), v(3, int64(i+1)), b(4, v(2, 1000000)), v(5, 1)))...)
		}
		call(t, app, "/hit_tracker.HitTrackerService/Track", batch)
	})
	require.Eventually(t, func() bool {
		_, err := f.Web.RPC("GetCacheScoreCard", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id})
		return err == nil
	}, 30*time.Second, 100*time.Millisecond)
	for _, transport := range []string{"http", "grpc"} {
		for sample := 0; sample < 2; sample++ {
			token := ""
			count := 0
			for page := 0; page < 2; page++ {
				if transport == "http" {
					out := web(t, f, "GetCacheScoreCard", map[string]any{"invocationId": id, "pageToken": token, "orderBy": "ORDER_BY_SIZE"})
					rows := out["results"].([]any)
					expected := 100
					if page == 1 {
						expected = 1
					}
					require.Len(t, rows, expected)
					for _, raw := range rows {
						count++
						require.Equal(t, int64(count), number(raw.(map[string]any)["digest"].(map[string]any)["sizeBytes"]))
					}
					token, _ = out["nextPageToken"].(string)
				} else {
					out := fields(t, call(t, app, "/buildbuddy.service.BuildBuddyService/GetCacheScoreCard", join(b(1, s(2, f.GroupID)), s(2, id), s(3, token), v(5, 3))))
					expected := 100
					if page == 1 {
						expected = 1
					}
					require.Len(t, out[2], expected)
					for _, raw := range out[2] {
						count++
						require.Equal(t, int64(count), number(string(one(fields(t, one(fields(t, raw), 7)), 2))))
					}
					token = string(one(out, 3))
				}
				if page == 0 {
					require.NotEmpty(t, token)
				} else {
					require.Empty(t, token)
				}
			}
			require.Equal(t, 101, count)
			time.Sleep(250 * time.Millisecond)
		}
	}
}

func enum(t *testing.T, value any, values map[string]int64) int64 {
	t.Helper()
	result, ok := values[fmt.Sprint(value)]
	require.True(t, ok, "unknown enum %v", value)
	return result
}

// Usage is a delayed, org-wide projection. Isolate each mutation so that a
// matching snapshot cannot hide a compensating error in a later operation.
func TestUsagePerStep(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"usage_tracking_enabled": true, "region": "fixture"}}}})
	admin := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "usage-admin", []string{"ORG_ADMIN"}))
	var batch []byte
	for i := 0; i < 8; i++ {
		d := harness.Digest(bytes.Repeat([]byte("u"), 10+i))
		digest, err := proto.Marshal(d)
		require.NoError(t, err)
		hit := join(b(2, join(b(1, digest), v(4, int64(1+i/4)), v(3, int64(i%2)))), v(3, d.SizeBytes), b(4, v(2, 1000000)), v(5, int64(1+(i/2)%2)))
		batch = append(batch, b(1, hit)...)
	}
	for step := 0; step < 3; step++ {
		started := time.Now()
		request := batch
		if step == 2 {
			request = nil
		}
		call(t, f.Clients(harness.App), "/hit_tracker.HitTrackerService/Track", request)
		count := int64(step + 1)
		if step == 2 {
			count = 2
		}
		require.Eventually(t, func() bool {
			out, err := f.Web.RPC("GetUsage", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
			if err != nil {
				return false
			}
			u, _ := out["usage"].(map[string]any)
			return number(u["casCacheHits"]) == 2*count && number(u["actionCacheHits"]) == 2*count && number(u["totalDownloadSizeBytes"]) == count*50 && number(u["totalUploadSizeBytes"]) == count*58
		}, 150*time.Second, time.Second)
		t.Logf("usage step=%d convergence<=%s", step, time.Since(started))
		for sample := 0; sample < 2; sample++ {
			usageBuckets(t, f, admin)
			u := web(t, f, "GetUsage", map[string]any{})["usage"].(map[string]any)
			for name, want := range map[string]int64{"casCacheHits": 2 * count, "actionCacheHits": 2 * count, "totalDownloadSizeBytes": count * 50, "totalUploadSizeBytes": count * 58} {
				require.Equal(t, want, number(u[name]), name)
			}
			grpcUsage := fields(t, one(fields(t, call(t, admin, "/buildbuddy.service.BuildBuddyService/GetUsage", b(1, s(2, f.GroupID)))), 2))
			for n, want := range map[protowire.Number]int64{3: 2 * count, 4: 2 * count, 5: count * 50, 7: count * 58} {
				require.Equal(t, want, number(string(one(grpcUsage, n))))
			}
			time.Sleep(time.Second)
		}
	}
}

func usageBuckets(t *testing.T, f *harness.Fixture, admin *harness.Clients) {
	t.Helper()
	out := web(t, f, "GetUsage", map[string]any{})
	monthly := out["usage"].(map[string]any)
	period := monthly["period"].(string)
	periods := out["availableUsagePeriods"].([]any)
	require.Contains(t, periods, period)
	daily := out["dailyUsage"].([]any)
	require.NotEmpty(t, daily)
	seen := map[string]bool{}
	sums := map[string]int64{}
	names := map[protowire.Number]string{3: "actionCacheHits", 4: "casCacheHits", 5: "totalDownloadSizeBytes", 7: "totalUploadSizeBytes", 8: "totalCachedActionExecUsec", 9: "totalExternalDownloadSizeBytes", 10: "totalInternalDownloadSizeBytes", 11: "totalWorkflowDownloadSizeBytes", 12: "totalExternalUploadSizeBytes", 13: "totalInternalUploadSizeBytes", 14: "totalWorkflowUploadSizeBytes", 20: "totalCustomerProxyDownloadSizeBytes", 21: "totalCustomerProxyUploadSizeBytes"}
	for _, raw := range daily {
		u := raw.(map[string]any)
		day := u["period"].(string)
		require.False(t, seen[day])
		seen[day] = true
		require.True(t, strings.HasPrefix(day, period+"-"))
		for _, name := range names {
			sums[name] += number(u[name])
		}
	}
	wire := fields(t, call(t, admin, "/buildbuddy.service.BuildBuddyService/GetUsage", b(1, s(2, f.GroupID))))
	grpcMonth := fields(t, one(wire, 2))
	require.Equal(t, period, string(one(grpcMonth, 1)))
	for n, name := range names {
		require.Equal(t, number(monthly[name]), sums[name], "daily sum %s", name)
		var total int64
		for _, raw := range wire[4] {
			total += number(string(one(fields(t, raw), n)))
		}
		require.Equal(t, sums[name], total, "gRPC daily sum %s", name)
		require.Equal(t, number(monthly[name]), number(string(one(grpcMonth, n))), "monthly %s", name)
	}
	explicit := web(t, f, "GetUsage", map[string]any{"usagePeriod": period})
	require.Equal(t, monthly, explicit["usage"])
}

func publicFileSurfaces(t *testing.T, ctx context.Context, f *harness.Fixture, uri string, payload []byte) {
	t.Helper()
	stream, err := f.Clients(harness.App).Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile", grpc.ForceCodec(codec{}))
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(s(1, uri)))
	require.NoError(t, stream.CloseSend())
	var data []byte
	for {
		var raw []byte
		err := stream.RecvMsg(&raw)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data = append(data, one(fields(t, raw), 1)...)
	}
	require.Equal(t, payload, data)
	out := fields(t, call(t, f.Clients(harness.App), "/api.v1.ApiService/GetFileRange", join(s(1, uri), v(2, 1), v(3, 4))))
	require.Equal(t, payload[1:4], one(out, 1))
	for _, method := range []string{"GetFile", "GetFileRange"} {
		body := map[string]any{"uri": uri}
		if method == "GetFileRange" {
			body["start"] = 1
			body["end"] = 4
		}
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(encoded))
		require.NoError(t, err)
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		require.NoError(t, err)
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, string(got))
		if method == "GetFileRange" {
			var decoded struct {
				Data []byte `json:"data"`
			}
			require.NoError(t, json.Unmarshal(got, &decoded))
			require.Equal(t, payload[1:4], decoded.Data)
		} else {
			require.Equal(t, payload, got)
		}
	}
}
