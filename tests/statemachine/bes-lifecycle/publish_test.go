package bes_lifecycle_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func fixtureRequests(t *testing.T, id string, success bool) []*bes.PublishBuildToolEventStreamRequest {
	sid := &bes.StreamId{BuildId: id, InvocationId: id, Component: bes.StreamId_TOOL}
	now := time.Now().UnixMilli()
	payloads := [][]byte{
		event(3, 5, nil, msg(str(1, id), num(2, now), str(3, "8.0.0"), str(4, "--color=no"), str(5, "test"), str(10, "fixture-host"), str(11, "fixture-user"))),
		event(14, 16, nil, msg(field(1, msg(str(1, "BUILD_USER"), str(2, "fixture-user"))), field(1, msg(str(1, "BUILD_HOST"), str(2, "fixture-host"))))),
		event(24, 26, nil, msg(field(1, msg(str(1, "ROLE"), str(2, "CI"))), field(1, msg(str(1, "REPO_URL"), str(2, "https://example.invalid/bes-lifecycle"))), field(1, msg(str(1, "COMMIT_SHA"), str(2, "0123456789abcdef"))), field(1, msg(str(1, "BRANCH_NAME"), str(2, "validation"))))),
		event(2, 3, num(1, 0), str(1, "state-machine console output\n")),
		msg(event(9, 14, nil, msg(field(3, str(1, "SUCCESS")), num(2, now+1500))), num(20, 1)),
	}
	if !success {
		payloads[4] = msg(event(9, 14, nil, msg(field(3, msg(str(1, "BUILD_FAILURE"), num(2, 1))), num(2, now+1500))), num(20, 1))
	}
	payloads[1], payloads[2] = payloads[2], payloads[1]
	payloads[0] = msg(payloads[0], field(2, field(14, nil)), field(2, field(24, nil)), field(2, field(2, num(1, 0))), field(2, field(9, nil)))

	requests := make([]*bes.PublishBuildToolEventStreamRequest, len(payloads))
	for i, b := range payloads {
		requests[i] = &bes.PublishBuildToolEventStreamRequest{ProjectId: "fixture", OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: sid, SequenceNumber: int64(i + 1), Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: b}}}}}
	}
	finalEvent := &bes.BuildEvent{}
	require.NoError(t, proto.Unmarshal(field(59, num(1, 1)), finalEvent)) // FINISHED component stream
	finalEvent.EventTime = timestamppb.Now()
	requests = append(requests, &bes.PublishBuildToolEventStreamRequest{ProjectId: "fixture", OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: sid, SequenceNumber: 6, Event: finalEvent}})
	return requests
}
func publishComplete(t *testing.T, ctx context.Context, c *harness.Clients, id string) []*bes.PublishBuildToolEventStreamRequest {
	t.Helper()
	s, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	requests := fixtureRequests(t, id, true)
	for _, r := range requests {
		require.NoError(t, s.Send(r))
	}
	require.NoError(t, s.CloseSend())
	for _, r := range requests {
		a, e := s.Recv()
		require.NoError(t, e)
		require.Equal(t, r.OrderedBuildEvent.SequenceNumber, a.SequenceNumber)
		require.True(t, proto.Equal(r.OrderedBuildEvent.StreamId, a.StreamId))
	}
	_, err = s.Recv()
	require.Equal(t, io.EOF, err)
	return requests
}
