package executionstate_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	lropb "google.golang.org/genproto/googleapis/longrunning"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

// Wire fields are from the released proto/{execution_stats,build_event_stream}.proto.
// Empty preserves unknown protobuf fields, avoiding source-generated dependencies.
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func number(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func envelope(b []byte) *emptypb.Empty {
	m := &emptypb.Empty{}
	m.ProtoReflect().SetUnknown(b)
	return m
}
func bytesField(t *testing.T, b []byte, want protowire.Number) []byte {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		size := protowire.ConsumeFieldValue(n, typ, b)
		require.GreaterOrEqual(t, size, 0)
		if n == want {
			require.Equal(t, protowire.BytesType, typ)
			v, k := protowire.ConsumeBytes(b)
			require.Positive(t, k)
			return v
		}
		b = b[size:]
	}
	return nil
}
func replayBB(t *testing.T, ctx context.Context, c *harness.Clients, name string, want *repb.ExecuteResponse) {
	t.Helper()
	stream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/WaitExecution")
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(envelope(field(2, []byte(name)))))
	require.NoError(t, stream.CloseSend())
	for {
		msg := &emptypb.Empty{}
		require.NoError(t, stream.RecvMsg(msg))
		op := &lropb.Operation{}
		require.NoError(t, proto.Unmarshal(bytesField(t, msg.ProtoReflect().GetUnknown(), 2), op))
		require.Equal(t, name, op.Name)
		if !op.Done {
			continue
		}
		got := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(got))
		require.True(t, responseEqual(want, got))
		return
	}
}
func grpcProjection(t *testing.T, ctx context.Context, c *harness.Clients, invocation, name string, want *repb.ExecuteResponse) {
	t.Helper()
	lookup := append(field(1, []byte(invocation)), field(3, []byte(name))...)
	req := envelope(append(field(2, lookup), number(3, 1)...))
	require.Eventually(t, func() bool {
		res := &emptypb.Empty{}
		if err := c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetExecution", req, res); err != nil {
			t.Log(err)
			return false
		}
		ex := bytesField(t, res.ProtoReflect().GetUnknown(), 2)
		if len(ex) == 0 {
			return false
		}
		if string(bytesField(t, ex, 14)) != name || stageField(t, ex) != uint64(repb.ExecutionStage_COMPLETED) {
			return false
		}
		got := &repb.ExecuteResponse{}
		b := bytesField(t, ex, 13)
		if len(b) == 0 {
			return false
		}
		require.NoError(t, proto.Unmarshal(b, got))
		return responseEqual(want, got)
	}, 30*time.Second, 100*time.Millisecond)
}
func startInvocation(t *testing.T, ctx context.Context, c *harness.Clients, id string) func() {
	t.Helper()
	bes, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	seq := int64(0)
	send := func(e *bespb.BuildEvent) {
		seq++
		require.NoError(t, bes.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: &bespb.StreamId{BuildId: id, InvocationId: id, Component: bespb.StreamId_TOOL}, SequenceNumber: seq, Event: e}}))
	}
	bazel := func(id, payload protowire.Number, body []byte, last bool) {
		b := append(field(1, field(id, nil)), field(payload, body)...)
		if last {
			b = append(b, number(20, 1)...)
		} else {
			b = append(b, field(2, field(9, nil))...)
		}
		send(&bespb.BuildEvent{EventTime: timestamppb.Now(), Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: b}}})
	}
	started := append(field(1, []byte(id)), number(2, uint64(time.Now().UnixMilli()))...)
	started = append(started, field(4, []byte("--remote_executor=grpc://app:1985"))...)
	started = append(started, field(5, []byte("test"))...)
	bazel(3, 5, started, false)
	return func() {
		finished := append(field(3, field(1, []byte("SUCCESS"))), number(2, uint64(time.Now().UnixMilli()))...)
		bazel(9, 14, finished, true)
		send(&bespb.BuildEvent{EventTime: timestamppb.Now(), Event: &bespb.BuildEvent_ComponentStreamFinished{ComponentStreamFinished: &bespb.BuildEvent_BuildComponentStreamFinished{}}})
		require.NoError(t, bes.CloseSend())
		for {
			_, err := bes.Recv()
			if err != nil {
				break
			}
		}
	}
}

// The release deliberately retains auxiliary stats and timelines in the UI
// cache while trimming them from REAPI streams/AC. Compare standard REAPI state.
func responseEqual(a, b *repb.ExecuteResponse) bool {
	clean := func(r *repb.ExecuteResponse) *repb.ExecuteResponse {
		r = proto.Clone(r).(*repb.ExecuteResponse)
		if md := r.GetResult().GetExecutionMetadata(); md != nil {
			md.AuxiliaryMetadata = nil
			md.ProtoReflect().SetUnknown(nil)
		}
		return r
	}
	return proto.Equal(clean(a), clean(b))
}

func stageField(t *testing.T, b []byte) uint64 {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		size := protowire.ConsumeFieldValue(n, typ, b)
		require.GreaterOrEqual(t, size, 0)
		if n == 2 {
			require.Equal(t, protowire.VarintType, typ)
			v, k := protowire.ConsumeVarint(b)
			require.Positive(t, k)
			return v
		}
		b = b[size:]
	}
	return 0
}
func executingGRPC(t *testing.T, ctx context.Context, c *harness.Clients, invocation, name string, action *repb.Digest) {
	t.Helper()
	require.Eventually(t, func() bool {
		lookup := append(field(1, []byte(invocation)), field(3, []byte(name))...)
		res := &emptypb.Empty{}
		if err := c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetExecution", envelope(field(2, lookup)), res); err != nil {
			return false
		}
		ex := bytesField(t, res.ProtoReflect().GetUnknown(), 2)
		if len(ex) == 0 {
			return false
		}
		got := &repb.Digest{}
		require.NoError(t, proto.Unmarshal(bytesField(t, ex, 1), got))
		return string(bytesField(t, ex, 14)) == name && stageField(t, ex) == uint64(repb.ExecutionStage_EXECUTING) && proto.Equal(action, got)
	}, 30*time.Second, 100*time.Millisecond)
}
