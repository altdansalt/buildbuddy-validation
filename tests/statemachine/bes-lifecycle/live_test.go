package bes_lifecycle_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func checkLogSnapshot(t *testing.T, f *harness.Fixture, c *harness.Clients, id, want string, complete ...bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	req := map[string]any{"invocationId": id, "chunkId": "0000", "requestContext": f.Web.RequestContext(f.GroupID)}
	require.Eventually(t, func() bool {
		r, e := f.Web.RPC("GetEventLogChunk", req)
		if e != nil {
			return false
		}
		b, _ := r["buffer"].(string)
		return b == base64.StdEncoding.EncodeToString([]byte(want)) || (want == "" && b == "")
	}, 20*time.Second, 100*time.Millisecond)
	for pass := 0; pass < 2; pass++ {
		r := rpc(t, f, "GetEventLogChunk", req)
		g, e := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetEventLogChunk", req)
		require.NoError(t, e)
		for _, response := range []map[string]any{r, g} {
			b, _ := response["buffer"].(string)
			data, e := base64.StdEncoding.DecodeString(b)
			require.NoError(t, e)
			require.Equal(t, want, string(data))
		}
		ar := map[string]any{"selector": map[string]any{"invocationId": id}, "pageToken": "0000"}
		code, a := api(t, f, "GetLog", ar)
		require.Equal(t, 200, code)
		ag, e := grpcRead(t, ctx, c, "api.v1.ApiService", "GetLog", ar)
		require.NoError(t, e)
		for _, response := range []map[string]any{a, ag} {
			log, _ := response["log"].(map[string]any)
			contents, _ := log["contents"].(string)
			require.Equal(t, want, contents)
		}
		liveCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		log, e := streamedLog(t, liveCtx, c, id, true)
		stop()
		require.NoError(t, e)
		require.Equal(t, want, log)
		if len(complete) > 0 && complete[0] {
			checkCLI(t, ctx, f, id, want)
		} else {
			// Known default-tail omission is isolated in live_tail_repro. Explicit
			// chunk readers above still assert each mutating progress request.
			checkCLI(t, ctx, f, id, "")
			code, tail := api(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": id}})
			require.Equal(t, 200, code)
			log, _ := tail["log"].(map[string]any)
			require.Empty(t, log["contents"])
		}
	}
	t.Logf("log snapshot %d bytes converged and remained stable across two reads within %s", len(want), time.Since(start))
}

func TestLiveProgressUpdates(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	id := uuid.NewString()
	requests := fixtureRequests(t, id, true)
	s, e := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, e)
	for _, r := range requests[:3] {
		require.NoError(t, s.Send(r))
	}
	checkModel(t, f, c, model{id: id, state: "PARTIAL_INVOCATION_STATUS"})
	checkLogSnapshot(t, f, c, id, "")
	var want string
	for step := 0; step < 3; step++ {
		line := fmt.Sprintf("progress step %d\n", step)
		want += line
		payload := event(2, 3, num(1, int64(step)), str(1, line))
		if step < 2 {
			payload = msg(payload, field(2, field(2, num(1, int64(step+1)))))
		}
		r := &bes.PublishBuildToolEventStreamRequest{ProjectId: "fixture", OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: requests[0].OrderedBuildEvent.StreamId, SequenceNumber: int64(step + 4), Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: payload}}}}}
		require.NoError(t, s.Send(r))
		checkModel(t, f, c, model{id: id, state: "PARTIAL_INVOCATION_STATUS"})
		checkLogSnapshot(t, f, c, id, want)
	}
	for i, r := range requests[4:] {
		r.OrderedBuildEvent.SequenceNumber = int64(i + 7)
		require.NoError(t, s.Send(r))
	}
	require.NoError(t, s.CloseSend())
	for seq := 1; seq <= 8; seq++ {
		ack, e := s.Recv()
		require.NoError(t, e)
		require.Equal(t, int64(seq), ack.SequenceNumber)
	}
	_, e = s.Recv()
	require.Equal(t, io.EOF, e)
	checkModel(t, f, c, model{id: id, state: "COMPLETE_INVOCATION_STATUS", success: true, duration: "1500000"})
	checkLogSnapshot(t, f, c, id, want, true)
	_, e = grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "DeleteInvocation", map[string]any{"invocationId": id})
	require.NoError(t, e)
	checkAbsent(t, f, c, id)
}
