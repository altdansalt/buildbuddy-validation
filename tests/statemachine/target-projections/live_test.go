package projections_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func webBytes(t *testing.T, f *harness.Fixture, path string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+path, nil)
	require.NoError(t, err)
	resp, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, "%s", b)
	return b
}

// Flush threshold is deliberately lowered to make every-event checks bounded
// without adding a minute of idle time per BES event. The normal-buffer fixture
// remains covered by TestSuccessiveBuildProjections.
func TestEveryEventProjection(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--app.enable_target_tracking=true", "--storage.chunk_file_size_bytes=1"}}})
	m := build(t, f, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stream, err := f.Clients(harness.App).BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	sid := &bes.StreamId{BuildId: m.id, InvocationId: m.id, Component: bes.StreamId_TOOL}
	for i, e := range m.events {
		require.NoError(t, stream.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: sid, SequenceNumber: int64(i + 1), Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: e}}}}}))
		// The released BES server ACKs only after EOF, so observe persistence
		// independently while the request stream remains open.
		started := time.Now()
		diagnostic := ""
		defer func() {
			if t.Failed() {
				t.Logf("live persistence diagnostic: %s", diagnostic)
			}
		}()
		require.Eventually(t, func() bool {
			req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?invocation_id="+m.id+"&artifact=raw_proto", nil)
			if err != nil {
				return false
			}
			resp, err := f.Web.Client.Do(req)
			if err != nil {
				return false
			}
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 {
				diagnostic = fmt.Sprintf("HTTP %d body=%s err=%v", resp.StatusCode, b, err)
				return false
			}
			var events [][]byte
			for len(b) > 0 {
				n, k := protowire.ConsumeVarint(b)
				if k < 0 || n > uint64(len(b)-k) {
					return false
				}
				b = b[k:]
				events = append(events, b[:n])
				b = b[n:]
			}
			if len(events) != i+1 {
				diagnostic = fmt.Sprintf("events=%d want=%d wire=%x", len(events), i+1, b)
				return false
			}
			for j, e := range events {
				for n := protowire.Number(1); n <= 30; n++ {
					want := values(m.events[j], n)
					if n == 2 && (len(values(m.events[j], 5)) > 0 || len(values(m.events[j], 6)) > 0) {
						want = nil
					} // lookup strips child references of started and expanded events only
					if len(values(m.events[j], 3)) > 0 && n == 3 {
						want = [][]byte{nil}
					} // logs are stored separately
					if j == 0 && n == 5 {
						want = [][]byte{msg(str(1, m.id), num(2, m.now), str(3, "8.0.0"), str(4, "<REDACTED>"), str(5, "test"))}
					}
					got := values(e, n)
					for k := range want {
						want[k] = canon(want[k])
					}
					for k := range got {
						got[k] = canon(got[k])
					}
					if n == 26 { // workspace status entries are a map: order is unspecified
						want, got = sortedBytes(want), sortedBytes(got)
					}
					if len(want) != len(got) || scalar(m.events[j], n) != scalar(e, n) {
						diagnostic = fmt.Sprintf("event=%d field=%d expected=%x got=%x scalar=%d/%d", j, n, want, got, scalar(m.events[j], n), scalar(e, n))
						return false
					}
					for k := range want {
						if !bytes.Equal(want[k], got[k]) {
							diagnostic = fmt.Sprintf("event=%d field=%d want=%x got=%x", j, n, want[k], got[k])
							return false
						}
					}
				}
			}
			return true
		}, 30*time.Second, 100*time.Millisecond, "event %d did not persist exactly", i+1)
		t.Logf("event=%d raw-proto convergence=%s", i+1, time.Since(started))
		// Raw JSON is the UI's all-events surface, including events omitted from
		// GetInvocation. Check exact sequence identities and wire projections.
		var raw []map[string]any
		require.NoError(t, json.Unmarshal(webBytes(t, f, "/file/download?invocation_id="+m.id+"&artifact=raw_json"), &raw))
		require.Len(t, raw, i+1)
		for j, r := range raw {
			require.NotEmpty(t, r["id"], "event %d", j)
		}
		inv := objects(t, rpc(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": m.id}}), "invocation")
		require.Len(t, inv, 1)
		require.Equal(t, m.id, inv[0].(map[string]any)["invocationId"])
		wire := readRPC(t, f, "/buildbuddy.service.BuildBuddyService/GetInvocation", field(2, str(1, m.id)))
		require.Len(t, values(wire, 2), 1)
		require.Equal(t, []byte(m.id), values(values(wire, 2)[0], 1)[0])
		code, apiInv := api(t, f, "GetInvocation", map[string]any{"selector": map[string]any{"invocationId": m.id}, "includeMetadata": true})
		require.Equal(t, 200, code)
		require.Len(t, objects(t, apiInv, "invocation"), 1)
		apiWire := readRPC(t, f, "/api.v1.ApiService/GetInvocation", field(1, str(1, m.id)))
		require.Len(t, values(apiWire, 1), 1)
		// Raw view and download must agree at each step, including partial streams.
		var viewed []map[string]any
		require.NoError(t, json.Unmarshal(webBytes(t, f, "/file/view?invocation_id="+m.id+"&artifact=raw_json"), &viewed))
		require.Equal(t, raw, viewed)
	}
	require.NoError(t, stream.CloseSend())
	for i := range m.events {
		ack, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, int64(i+1), ack.SequenceNumber)
	}
	_, err = stream.Recv()
	require.Equal(t, io.EOF, err)
	binary, dir := releasedCLI(t, f)
	check(t, f, m, binary, dir)
	checkHistory(t, f, []buildModel{m})
	checkSearch(t, f, []buildModel{m})
}

func checkSearch(t *testing.T, f *harness.Fixture, model []buildModel) {
	t.Helper()
	expected := map[string]bool{}
	for _, m := range model {
		expected[m.id] = m.step != 1
	}
	start := time.Now()
	var got map[string]bool
	require.Eventually(t, func() bool {
		res, err := f.Web.RPC("SearchInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "query": map[string]any{"repoUrl": "https://example.invalid/projections"}, "count": 1})
		if err != nil {
			return false
		}
		got = map[string]bool{}
		seen := map[string]bool{}
		for {
			for _, v := range objects(t, res, "invocation") {
				in := v.(map[string]any)
				id := in["invocationId"].(string)
				if _, ok := got[id]; ok {
					return false
				}
				got[id] = in["success"] == true
			}
			token, _ := res["nextPageToken"].(string)
			if token == "" {
				break
			}
			if seen[token] {
				return false
			}
			seen[token] = true
			res, err = f.Web.RPC("SearchInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "query": map[string]any{"repoUrl": "https://example.invalid/projections"}, "count": 1, "pageToken": token})
			if err != nil {
				return false
			}
		}
		return reflect.DeepEqual(expected, got)
	}, 30*time.Second, 100*time.Millisecond)
	t.Logf("SearchInvocation convergence=%s", time.Since(start))
	// Recheck after the async stats finalization delay to catch regressions.
	timer := time.NewTimer(600 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	res := rpc(t, f, "SearchInvocation", map[string]any{"query": map[string]any{"repoUrl": "https://example.invalid/projections"}, "count": 100})
	stable := map[string]bool{}
	for _, v := range objects(t, res, "invocation") {
		in := v.(map[string]any)
		stable[in["invocationId"].(string)] = in["success"] == true
	}
	require.Equal(t, expected, stable)
	stats := rpc(t, f, "GetInvocationStat", map[string]any{"aggregationType": "REPO_URL_AGGREGATION_TYPE", "query": map[string]any{"repoUrl": "https://example.invalid/projections"}})
	rows := objects(t, stats, "invocationStat")
	require.Len(t, rows, 1)
	require.Equal(t, fmt.Sprint(len(model)), fmt.Sprint(rows[0].(map[string]any)["totalNumBuilds"]))
	wire := readRPC(t, f, "/buildbuddy.service.BuildBuddyService/GetInvocationStat", msg(field(1, str(2, f.GroupID)), num(2, 4), field(4, str(4, "https://example.invalid/projections"))))
	require.Len(t, values(wire, 2), 1)
	require.Equal(t, uint64(len(model)), scalar(values(wire, 2)[0], 4))
}

// canon re-orders wire fields by number (recursively where a bytes field parses
// as a message) so semantically equal encodings compare equal. The heuristic is
// applied identically to expected and stored bytes.
func canon(b []byte) []byte {
	type fld struct {
		n   protowire.Number
		raw []byte
	}
	var fs []fld
	rest := b
	for len(rest) > 0 {
		n, typ, k := protowire.ConsumeTag(rest)
		if k < 0 || n < 1 {
			return b
		}
		l := protowire.ConsumeFieldValue(n, typ, rest[k:])
		if l < 0 {
			return b
		}
		raw := rest[:k+l]
		if typ == protowire.VarintType {
			if v, _ := protowire.ConsumeVarint(rest[k:]); v == 0 {
				rest = rest[k+l:] // proto3 re-marshaling drops zero-valued scalars
				continue
			}
		}
		if typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(rest[k:])
			raw = protowire.AppendBytes(append([]byte(nil), rest[:k]...), canon(v))
		}
		fs = append(fs, fld{n, raw})
		rest = rest[k+l:]
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].n < fs[j].n })
	var out []byte
	for _, f := range fs {
		out = append(out, f.raw...)
	}
	return out
}

func sortedBytes(in [][]byte) [][]byte {
	if len(in) == 1 { // one message holding repeated entries: split and sort them
		in = values(in[0], 1)
	}
	out := append([][]byte(nil), in...)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

func streamedFile(t *testing.T, f *harness.Fixture, uri string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := f.Clients(harness.App).Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	req := str(1, uri)
	require.NoError(t, s.SendMsg(&req))
	require.NoError(t, s.CloseSend())
	var all []byte
	for {
		var b []byte
		err := s.RecvMsg(&b)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		for _, data := range values(b, 1) {
			all = append(all, data...)
		}
	}
	return all
}
