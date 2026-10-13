package projections_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Field numbers follow proto/build_event_stream.proto at v2.314.0. Encoding
// these few messages directly avoids adding the entire BuildBuddy module.
func msg(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func str(n protowire.Number, s string) []byte { return field(n, []byte(s)) }
func num(n protowire.Number, v int64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), uint64(v))
}
func event(id, payload protowire.Number, identity, data []byte) []byte {
	return msg(field(1, field(id, identity)), field(payload, data))
}

func publish(t *testing.T, f *harness.Fixture, id string, events [][]byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stream, err := f.Clients(harness.App).BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	sid := &bes.StreamId{BuildId: id, InvocationId: id, Component: bes.StreamId_TOOL}
	for i, b := range events {
		require.NoError(t, stream.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: sid, SequenceNumber: int64(i + 1), Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: b}}}}}))
	}
	require.NoError(t, stream.CloseSend())
	for i := range events {
		ack, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, int64(i+1), ack.SequenceNumber)
	}
	_, err = stream.Recv()
	require.Equal(t, io.EOF, err)
}

func rpc(t *testing.T, f *harness.Fixture, method string, req map[string]any) map[string]any {
	t.Helper()
	req["requestContext"] = f.Web.RequestContext(f.GroupID)
	res, err := f.Web.RPC(method, req)
	require.NoError(t, err)
	return res
}
func api(t *testing.T, f *harness.Fixture, method string, req map[string]any) (int, map[string]any) {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	r, err := http.NewRequest("POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(b))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-buildbuddy-api-key", f.APIKey)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(r)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, map[string]any{"error": string(body)}
	}
	var result map[string]any
	require.NoError(t, json.Unmarshal(body, &result), "HTTP %d: %s", resp.StatusCode, body)
	return resp.StatusCode, result
}
func objects(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	if m[key] == nil {
		return nil
	}
	a, ok := m[key].([]any)
	require.True(t, ok, "%v", m)
	return a
}
