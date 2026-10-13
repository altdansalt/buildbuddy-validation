package bes_lifecycle_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/grpcreflect"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

func streamedLog(t *testing.T, ctx context.Context, c *harness.Clients, id string, firstOnly ...bool) (string, error) {
	t.Helper()
	rc := grpcreflect.NewClientAuto(ctx, c.Conn)
	defer rc.Reset()
	sd, e := rc.ResolveService("buildbuddy.service.BuildBuddyService")
	require.NoError(t, e)
	md := sd.FindMethodByName("GetEventLog")
	in := dynamic.NewMessage(md.GetInputType())
	require.NoError(t, in.UnmarshalJSON([]byte(`{"invocationId":"`+id+`","chunkId":"0000"}`)))
	stream, e := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetEventLog")
	if e != nil {
		return "", e
	}
	require.NoError(t, stream.SendMsg(in))
	require.NoError(t, stream.CloseSend())
	var contents string
	chunks := 0
	for {
		out := dynamic.NewMessage(md.GetOutputType())
		e = stream.RecvMsg(out)
		if e == io.EOF {
			return contents, nil
		}
		if e != nil {
			return contents, e
		}
		b, e := out.MarshalJSON()
		require.NoError(t, e)
		var r map[string]any
		require.NoError(t, json.Unmarshal(b, &r))
		if encoded, ok := r["buffer"].(string); ok {
			data, e := base64.StdEncoding.DecodeString(encoded)
			require.NoError(t, e)
			contents += string(data)
		}
		if len(firstOnly) > 0 && firstOnly[0] {
			return contents, nil
		}
		chunks++
		require.LessOrEqual(t, chunks, 2, "data chunk followed by terminal empty chunk")
	}
}

func checkGraph(t *testing.T, f *harness.Fixture, c *harness.Clients, requests []*bes.PublishBuildToolEventStreamRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := requests[0].OrderedBuildEvent.StreamId.InvocationId
	rc := grpcreflect.NewClientAuto(ctx, c.Conn)
	defer rc.Reset()
	desc, e := rc.ResolveMessage("build_event_stream.BuildEvent")
	require.NoError(t, e)
	var want []any
	for _, req := range requests {
		payload := req.OrderedBuildEvent.Event.GetBazelEvent()
		if payload == nil {
			continue
		}
		m := dynamic.NewMessage(desc)
		require.NoError(t, m.Unmarshal(payload.Value))
		if p := m.GetFieldByName("progress"); p != nil {
			if progress, ok := p.(*dynamic.Message); ok && progress != nil {
				progress.ClearFieldByName("stdout")
				progress.ClearFieldByName("stderr")
			}
		}
		b, e := m.MarshalJSON()
		require.NoError(t, e)
		var event map[string]any
		require.NoError(t, json.Unmarshal(b, &event))
		if started, ok := event["started"].(map[string]any); ok {
			started["optionsDescription"] = "<REDACTED>"
			delete(event, "children")
		}
		want = append(want, event)
	}
	lookup := map[string]any{"lookup": map[string]any{"invocationId": id}, "requestContext": f.Web.RequestContext(f.GroupID)}
	ui := rpc(t, f, "GetInvocation", lookup)
	direct, e := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetInvocation", lookup)
	require.NoError(t, e)
	for name, response := range map[string]map[string]any{"UI": ui, "gRPC": direct} {
		inv := objects(t, response, "invocation")[0].(map[string]any)
		var got []any
		for _, ev := range objects(t, inv, "event") {
			got = append(got, ev.(map[string]any)["buildEvent"])
		}
		var detailWant []any
		for _, ev := range want {
			data, e := json.Marshal(ev)
			require.NoError(t, e)
			var copy map[string]any
			require.NoError(t, json.Unmarshal(data, &copy))
			delete(copy, "children")
			if _, progress := copy["progress"]; !progress {
				detailWant = append(detailWant, copy)
			}
		}
		require.Equal(t, detailWant, got, name+" full event graph")
	}
	for _, route := range []string{"download", "view"} {
		code, b := artifactFetch(t, ctx, f, id, "raw_json", route)
		require.Equal(t, 200, code)
		var got []any
		require.NoError(t, json.Unmarshal(b, &got))
		require.Equal(t, want, got, route+" raw JSON")
		code, b = artifactFetch(t, ctx, f, id, "raw_proto", route)
		require.Equal(t, 200, code)
		var raw []any
		for len(b) > 0 {
			n, k := protowire.ConsumeVarint(b)
			require.Positive(t, k)
			b = b[k:]
			require.LessOrEqual(t, n, uint64(len(b)))
			m := dynamic.NewMessage(desc)
			require.NoError(t, m.Unmarshal(b[:int(n)]))
			b = b[int(n):]
			data, e := m.MarshalJSON()
			require.NoError(t, e)
			var event map[string]any
			require.NoError(t, json.Unmarshal(data, &event))
			raw = append(raw, event)
		}
		require.Equal(t, want, raw, route+" raw proto")
		code, b = artifactFetch(t, ctx, f, id, "buildlog", route)
		require.Equal(t, 200, code)
		require.Equal(t, "state-machine console output\n", string(b))
	}
	log, e := streamedLog(t, ctx, c, id)
	require.NoError(t, e)
	require.Equal(t, "state-machine console output\n", log)
	chunk := rpc(t, f, "GetEventLogChunk", map[string]any{"invocationId": id, "minLines": 100})
	require.Equal(t, "0001", chunk["nextChunkId"])
	tail := rpc(t, f, "GetEventLogChunk", map[string]any{"invocationId": id, "chunkId": "0001"})
	require.Empty(t, tail["buffer"])
	require.Empty(t, tail["nextChunkId"])
	require.Equal(t, "0000", tail["previousChunkId"])
	require.Empty(t, chunk["previousChunkId"])
	require.NotEqual(t, true, chunk["live"])
}

func checkAbsent(t *testing.T, f *harness.Fixture, c *harness.Clients, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, e := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})
	require.Equal(t, codes.NotFound, status.Code(e))
	_, e = f.Web.RPC("GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})
	require.ErrorContains(t, e, "NotFound")
	for _, selector := range []map[string]any{{"invocationId": id}, {"commitSha": "0123456789abcdef"}} {
		req := map[string]any{"selector": selector}
		code, r := api(t, f, "GetInvocation", req)
		require.Equal(t, 200, code)
		require.Empty(t, objects(t, r, "invocation"))
		r, e := grpcRead(t, ctx, c, "api.v1.ApiService", "GetInvocation", req)
		require.NoError(t, e)
		require.Empty(t, objects(t, r, "invocation"))
	}
	_, e = streamedLog(t, ctx, c, id)
	require.Equal(t, codes.NotFound, status.Code(e))
}

// Same ID is removed and created anew, using two independently issued keys in
// the same organization. The second client deletes state written by the first.
func TestDeleteRecreateTwoKeys(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	first := f.Clients(harness.App)
	key := f.Web.CreateAPIKey(t, f.GroupID, "second-bes-client", []string{"CACHE_WRITE"})
	second := harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	id := uuid.NewString()
	for generation := 0; generation < 2; generation++ {
		writer, reader := first, second
		if generation == 1 {
			writer, reader = second, first
		}
		start := time.Now()
		requests := publishComplete(t, ctx, writer, id)
		checkModel(t, f, reader, model{id: id, state: "COMPLETE_INVOCATION_STATUS", success: true, duration: "1500000"})
		checkGraph(t, f, reader, requests)
		checkCLI(t, ctx, f, id, "state-machine console output\n")
		t.Logf("generation %d publish-to-verified convergence: %s", generation, time.Since(start))
		checkModel(t, f, reader, model{id: id, state: "COMPLETE_INVOCATION_STATUS", success: true, duration: "1500000"})
		_, e := grpcRead(t, ctx, reader, "buildbuddy.service.BuildBuddyService", "DeleteInvocation", map[string]any{"invocationId": id})
		require.NoError(t, e)
		checkAbsent(t, f, first, id)
		checkAbsent(t, f, second, id)
	}
}

func checkProxyInvocationBoundary(t *testing.T, ctx context.Context, app, proxy *harness.Clients, id string) {
	t.Helper()
	rc := grpcreflect.NewClientAuto(ctx, app.Conn)
	defer rc.Reset()
	sd, e := rc.ResolveService("buildbuddy.service.BuildBuddyService")
	require.NoError(t, e)
	for _, method := range []string{"GetInvocation", "DeleteInvocation"} {
		md := sd.FindMethodByName(method)
		in, out := dynamic.NewMessage(md.GetInputType()), dynamic.NewMessage(md.GetOutputType())
		req := map[string]any{"invocationId": id}
		if method == "GetInvocation" {
			req = map[string]any{"lookup": map[string]any{"invocationId": id}}
		}
		b, e := json.Marshal(req)
		require.NoError(t, e)
		require.NoError(t, in.UnmarshalJSON(b))
		e = proxy.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, in, out)
		require.Equal(t, codes.Unimplemented, status.Code(e))
	}
}
