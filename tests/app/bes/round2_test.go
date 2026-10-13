package bes_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func invocation(f *harness.Fixture, id string) (map[string]any, error) {
	r, err := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
	if err != nil {
		return nil, err
	}
	v, ok := r["invocation"].([]any)
	if !ok || len(v) != 1 {
		return nil, fmt.Errorf("missing invocation: %v", r)
	}
	return v[0].(map[string]any), nil
}

func eventLog(t *testing.T, f *harness.Fixture, id string) string {
	t.Helper()
	r, err := f.Web.RPC("GetEventLogChunk", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id, "minLines": 1000})
	if err != nil {
		t.Fatal(err)
	}
	buffer, _ := r["buffer"].(string)
	b, err := base64.StdEncoding.DecodeString(buffer)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRound2(t *testing.T) {
	f := startApp(t)
	c := f.Clients(harness.App)
	t.Run("interrupted_full_replay", func(t *testing.T) {
		// Replayable random cut points; no ACK is assumed before the RPC ends.
		const seed = int64(20261012)
		t.Logf("seed=%d", seed)
		rng := rand.New(rand.NewSource(seed))
		for trial := 0; trial < 4; trial++ {
			id := uuid.NewString()
			reqs := []*bespb.PublishBuildToolEventStreamRequest{request(id, 1, started())}
			for seq := int64(2); seq <= 8; seq++ {
				reqs = append(reqs, request(id, seq, progress(fmt.Sprintf("trial=%d seq=%d\n", trial, seq))))
			}
			reqs = append(reqs, request(id, 9, finished()))
			cut := 1 + rng.Intn(len(reqs)-1)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			s, err := c.BES.PublishBuildToolEventStream(ctx)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			for _, r := range reqs[:cut] {
				if err := s.Send(r); err != nil {
					cancel()
					t.Fatal(err)
				}
			}
			// Readback establishes the server handled the prefix before interruption.
			deadline := time.Now().Add(10 * time.Second)
			for {
				_, err = invocation(f, id)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					cancel()
					t.Fatalf("prefix not persisted: %v", err)
				}
				time.Sleep(100 * time.Millisecond)
			}
			cancel()
			_, err = s.Recv()
			if status.Code(err) != codes.Canceled {
				t.Fatalf("cancel: %v", err)
			}
			// Retry transient competing-channel errors until the canceled channel closes.
			deadline = time.Now().Add(10 * time.Second)
			for {
				acks, err := publish(t, c, reqs...)
				if err == nil {
					if len(acks) != len(reqs) {
						t.Fatalf("replay ACKs=%v", acks)
					}
					for i, ack := range acks {
						if ack.SequenceNumber != int64(i+1) || !proto.Equal(ack.StreamId, reqs[i].OrderedBuildEvent.StreamId) {
							t.Fatalf("replay ACK=%v", ack)
						}
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(100 * time.Millisecond)
			}
			inv, err := invocation(f, id)
			if err != nil || inv["success"] != true {
				t.Fatalf("trial=%d cut=%d readback=%v error=%v", trial, cut, inv, err)
			}
			logs := eventLog(t, f, id)
			for seq := 2; seq <= 8; seq++ {
				token := fmt.Sprintf("trial=%d seq=%d\n", trial, seq)
				if strings.Count(logs, token) != 1 {
					t.Fatalf("trial=%d cut=%d token=%q logs=%q; want each replayed event exactly once", trial, cut, token, logs)
				}
			}
		}
	})
	t.Run("invalid_stream_codes", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			req  *bespb.PublishBuildToolEventStreamRequest
		}{
			{"missing_envelope", &bespb.PublishBuildToolEventStreamRequest{}},
			{"malformed_wire", request(uuid.NewString(), 1, []byte{0xff})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				acks, err := publish(t, c, tc.req)
				t.Logf("code=%s error=%v", status.Code(err), err)
				if len(acks) != 0 || err == nil {
					t.Fatalf("ACKs=%v error=%v", acks, err)
				}
			})
		}
	})
}

func TestMessageSizeBoundary(t *testing.T) {
	// The released default receive limit is 50,000,000 bytes. Measure the entire
	// protobuf envelope rather than assuming the payload length equals wire size.
	f := startApp(t)
	c := f.Clients(harness.App)
	const limit = 50_000_000
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprint(delta), func(t *testing.T) {
			id := uuid.NewString()
			n := limit - 200
			r := request(id, 2, progress(strings.Repeat("x", n)))
			n += limit + delta - proto.Size(r)
			r = request(id, 2, progress(strings.Repeat("x", n)))
			if proto.Size(r) != limit+delta {
				t.Fatalf("wire size=%d", proto.Size(r))
			}
			acks, err := publish(t, c, request(id, 1, started()), r, request(id, 3, finished()))
			if delta > 0 {
				if status.Code(err) != codes.ResourceExhausted || len(acks) != 0 {
					t.Fatalf("oversize ACKs=%v error=%v", acks, err)
				}
				checkACKs(t, c, request(uuid.NewString(), 1, started()))
				return
			}
			if err != nil || len(acks) != 3 {
				t.Fatalf("size=%d ACKs=%v error=%v", proto.Size(r), acks, err)
			}
			inv, err := invocation(f, id)
			if err != nil || inv["success"] != true {
				t.Fatalf("readback=%v error=%v", inv, err)
			}
		})
	}
}
