package runlogs_test

import (
	"bytes"
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"io"
	"testing"
	"time"
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

func upload(t *testing.T, f *harness.Fixture, logs []string, exit int64) string {
	return uploadMetadata(t, f, logs, exit, nil)
}
func uploadMetadata(t *testing.T, f *harness.Fixture, logs []string, exit int64, extra [][2]string) string {
	t.Helper()
	id := uuid.NewString()
	now := time.Now().UnixMilli()
	events := [][]byte{event(3, 5, nil, msg(str(1, id), num(2, now), str(3, "8.0.0"), str(4, "--color=no"), str(5, "test"), str(10, "fixture-host"), str(11, "fixture-user")))}
	metadata := []byte{}
	for _, kv := range extra {
		metadata = append(metadata, field(1, msg(str(1, kv[0]), str(2, kv[1])))...)
	}
	for _, kv := range [][2]string{{"REPO_URL", "https://example.invalid/conformance"}, {"COMMIT_SHA", "0123456789abcdef"}, {"BRANCH_NAME", "validation"}, {"ROLE", "CI"}, {"custom-key", "custom-value"}} {
		metadata = append(metadata, field(1, msg(str(1, kv[0]), str(2, kv[1])))...)
	}
	events = append(events, event(14, 16, nil, msg(field(1, msg(str(1, "BUILD_USER"), str(2, "fixture-user"))), field(1, msg(str(1, "BUILD_HOST"), str(2, "fixture-host"))))), event(24, 26, nil, metadata), event(4, 6, str(1, "//sample:test"), nil), event(16, 18, str(1, "//sample:test"), msg(str(1, "sh_test rule"), num(2, 1), str(3, "validation"))), event(5, 8, str(1, "//sample:test"), num(1, 1)), event(8, 10, msg(str(1, "//sample:test"), num(2, 1), num(4, 1)), msg(num(5, 1), num(3, 125))), event(7, 9, str(1, "//sample:test"), msg(num(5, 1), num(1, 1), num(10, 1), num(11, 1))))
	for i, s := range logs {
		events = append(events, event(2, 3, num(1, int64(i)), str(1, s)))
	}
	name := "SUCCESS"
	if exit != 0 {
		name = "BUILD_FAILURE"
	}
	events = append(events, msg(event(9, 14, nil, msg(field(3, msg(str(1, name), num(2, exit))), num(2, now+1500))), num(20, 1)))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	return id
}
