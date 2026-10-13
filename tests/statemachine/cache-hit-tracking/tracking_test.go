package tracking_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
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
func number(x any) int64 { n, _ := strconv.ParseInt(fmt.Sprint(x), 10, 64); return n }
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
			require.Equal(t, "Compile", string(one(m, 1)))
			require.Equal(t, "//fixture:target", string(one(m, 2)))
			require.Equal(t, "action", string(one(m, 3)))
		}
		for _, raw := range httpRows {
			m := raw.(map[string]any)
			d := m["digest"].(map[string]any)
			ct := map[string]int64{"AC": 1, "CAS": 2}[fmt.Sprint(m["cacheType"])]
			rt := map[string]int64{"READ": 1, "WRITE": 2}[fmt.Sprint(m["requestType"])]
			comp := map[string]int64{"ZSTD": 1}[fmt.Sprint(m["compressor"])]
			httpActual[row{fmt.Sprint(d["hash"]), number(d["sizeBytes"]), number(m["transferredSizeBytes"]), ct, rt, comp}]++
			require.Equal(t, "Compile", m["actionMnemonic"])
			require.Equal(t, "//fixture:target", m["targetId"])
			require.Equal(t, "action", m["actionId"])
		}
		require.Equal(t, want, actual)
		require.Equal(t, want, httpActual)

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
		for n, name := range map[protowire.Number]string{1: "actionCacheHits", 3: "actionCacheUploads", 4: "casCacheHits", 6: "casCacheUploads", 7: "totalDownloadSizeBytes", 8: "totalUploadSizeBytes", 14: "totalDownloadTransferredSizeBytes", 15: "totalUploadTransferredSizeBytes"} {
			require.Equal(t, statsWant[n], number(string(one(stats, n))), name)
			require.Equal(t, statsWant[n], number(httpStats[name]), name)
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
}
