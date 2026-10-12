package bes_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Inspect the harness-preserved logs after it closes only this fixture.
func startApp(t *testing.T) *harness.Fixture {
	t.Helper()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	t.Cleanup(func() {
		container := f.Endpoint(harness.App).Container
		f.Close()
		data, err := os.ReadFile(filepath.Join(f.OutputDir, container+".logs.txt"))
		if err != nil {
			t.Errorf("read preserved app log: %v", err)
			return
		}
		logs := string(data)
		if strings.Contains(logs, "panic:") || strings.Contains(logs, "fatal error:") {
			t.Error("app panicked; see preserved container logs")
		}
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, "Voiding EventChannel") || strings.Contains(line, "Missing ack:") || strings.Contains(line, "error reading bazel event:") {
				t.Logf("app log: %s", line)
			}
		}
	})
	return f
}

// Encode the small subset of the pinned Bazel BuildEvent schema needed here.
// This avoids adding a shared dependency on BuildBuddy's generated Go protos.
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func started() []byte {
	p := append(field(3, []byte("8.0.0")), field(4, []byte("--bes_upload_mode=wait_for_upload_complete"))...)
	p = append(p, field(5, []byte("build"))...)
	return append(field(1, field(3, nil)), field(5, p)...)
}
func progress(s string) []byte {
	return append(field(1, field(2, nil)), field(3, field(1, []byte(s)))...)
}
func finished() []byte {
	b := append(field(1, field(9, nil)), field(14, field(3, field(1, []byte("SUCCESS"))))...)
	return protowire.AppendVarint(protowire.AppendTag(b, 20, protowire.VarintType), 1)
}
func request(id string, seq int64, payload []byte) *bespb.PublishBuildToolEventStreamRequest {
	return &bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{
		StreamId: &bespb.StreamId{InvocationId: id, BuildId: id, Component: bespb.StreamId_TOOL}, SequenceNumber: seq,
		Event: &bespb.BuildEvent{Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: payload}}}}}
}
func publish(t *testing.T, c *harness.Clients, reqs ...*bespb.PublishBuildToolEventStreamRequest) ([]*bespb.PublishBuildToolEventStreamResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := c.BES.PublishBuildToolEventStream(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range reqs {
		if err := s.Send(r); err != nil && err != io.EOF {
			return nil, err
		}
	}
	if err := s.CloseSend(); err != nil {
		return nil, err
	}
	var acks []*bespb.PublishBuildToolEventStreamResponse
	for {
		r, err := s.Recv()
		if err == io.EOF {
			return acks, nil
		}
		if err != nil {
			return acks, err
		}
		acks = append(acks, r)
	}
}
func checkACKs(t *testing.T, c *harness.Clients, reqs ...*bespb.PublishBuildToolEventStreamRequest) {
	t.Helper()
	acks, err := publish(t, c, reqs...)
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != len(reqs) {
		t.Fatalf("ACK count %d, want %d", len(acks), len(reqs))
	}
	for i, a := range acks {
		if a.SequenceNumber != reqs[i].OrderedBuildEvent.SequenceNumber || !proto.Equal(a.StreamId, reqs[i].OrderedBuildEvent.StreamId) {
			t.Fatalf("ACK %d: %v", i, a)
		}
	}
}
func TestRegression(t *testing.T) {
	f := startApp(t)
	c := f.Clients(harness.App)
	t.Run("complete_and_retry", func(t *testing.T) {
		id := uuid.NewString()
		r := []*bespb.PublishBuildToolEventStreamRequest{request(id, 1, started()), request(id, 2, progress("hello")), request(id, 3, finished())}
		checkACKs(t, c, r...)
		checkACKs(t, c, r...)
		checkACKs(t, c, r[2])
		result, err := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
		if err != nil {
			t.Fatal(err)
		}
		invocations, ok := result["invocation"].([]any)
		if !ok || len(invocations) != 1 {
			t.Fatalf("completed upload missing from app: %v", result)
		}
		inv := invocations[0].(map[string]any)
		if inv["invocationId"] != id || inv["success"] != true {
			t.Fatalf("incorrect completed invocation: %v", inv)
		}
	})

	t.Run("out_of_order", func(t *testing.T) {
		id := uuid.NewString()
		acks, err := publish(t, c, request(id, 1, started()), request(id, 3, progress("third")), request(id, 2, progress("second")), request(id, 4, finished()))
		if err != nil {
			t.Fatal(err)
		}
		if len(acks) != 4 {
			t.Fatalf("ACK count %d, want 4", len(acks))
		}
		for i, ack := range acks {
			if ack.SequenceNumber != int64(i+1) || !proto.Equal(ack.StreamId, request(id, 1, nil).OrderedBuildEvent.StreamId) {
				t.Fatalf("ACK %d: %v", i, ack)
			}
		}
	})
	t.Run("missing_envelope", func(t *testing.T) {
		acks, err := publish(t, c, &bespb.PublishBuildToolEventStreamRequest{})
		if err == nil || len(acks) != 0 {
			t.Fatalf("empty request ACKs=%v error=%v", acks, err)
		}
	})
	t.Run("concurrent_same_invocation", func(t *testing.T) {
		id := uuid.NewString()
		done := make(chan error, 2)
		gate := make(chan struct{})
		for i := 0; i < 2; i++ {
			go func() {
				<-gate
				_, err := publish(t, c, request(id, 1, started()), request(id, 2, finished()))
				done <- err
			}()
		}
		close(gate)
		first, second := <-done, <-done
		if first != nil && second != nil {
			t.Fatalf("both competing streams failed: %v; %v", first, second)
		}
		checkACKs(t, c, request(id, 1, started()), request(id, 2, finished()))
	})
	t.Run("empty", func(t *testing.T) { checkACKs(t, c) })
	t.Run("missing_finish", func(t *testing.T) { checkACKs(t, c, request(uuid.NewString(), 1, started())) })
	t.Run("missing_start", func(t *testing.T) { checkACKs(t, c, request(uuid.NewString(), 1, progress("no start"))) })
	t.Run("large_progress", func(t *testing.T) {
		id := uuid.NewString()
		checkACKs(t, c, request(id, 1, started()), request(id, 2, progress(strings.Repeat("x", 8<<20))), request(id, 3, finished()))
	})
	for _, tc := range []struct {
		name string
		seq  []int64
	}{{"gap", []int64{1, 3}}, {"duplicate", []int64{1, 1}}} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.NewString()
			var r []*bespb.PublishBuildToolEventStreamRequest
			for _, n := range tc.seq {
				r = append(r, request(id, n, started()))
			}
			acks, err := publish(t, c, r...)
			if err == nil || len(acks) != 0 {
				t.Fatalf("invalid sequence ACKs=%v error=%v", acks, err)
			}
		})
	}
	t.Run("malformed_wire", func(t *testing.T) {
		acks, err := publish(t, c, request(uuid.NewString(), 1, []byte{0xff}))
		if err == nil || len(acks) != 0 {
			t.Fatalf("malformed payload ACKs=%v error=%v", acks, err)
		}
		checkACKs(t, c, request(uuid.NewString(), 1, started()))
	})
}
