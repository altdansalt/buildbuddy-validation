package iprules_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/types/known/anypb"
)

const logMarker = "ip-rule private invocation log marker\n"

func seedInvocation(t *testing.T, c *harness.Clients, ctx context.Context) string {
	t.Helper()
	id := fmt.Sprintf("%08x-0000-4000-8000-%012x", uint32(time.Now().UnixNano()), uint64(time.Now().UnixNano())&0xffffffffffff)
	started := append(field(1, field(3, nil)), field(5, append(field(4, []byte("--bes_upload_mode=wait_for_upload_complete")), field(5, []byte("build"))...))...)
	progress := append(field(1, field(2, nil)), field(3, field(1, []byte(logMarker)))...)
	finished := append(append(field(1, field(9, nil)), field(14, field(3, field(1, []byte("SUCCESS"))))...), 0xa0, 0x01, 0x01)
	stream, err := c.BES.PublishBuildToolEventStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, payload := range [][]byte{started, progress, finished} {
		if err := stream.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: &bespb.StreamId{InvocationId: id, BuildId: id, Component: bespb.StreamId_TOOL}, SequenceNumber: int64(i + 1), Event: &bespb.BuildEvent{Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: payload}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 3 {
		t.Fatalf("BES ACK count=%d", count)
	}
	return id
}
