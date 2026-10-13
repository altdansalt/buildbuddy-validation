package randbes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var seed = flag.Int64("rand_bes.seed", 314159, "Replayable random seed")
var iterations = flag.Int("rand_bes.iterations", 12, "Number of streams (1..200)")

// Wire field numbers are from BuildBuddy v2.314.0 proto/build_event_stream.proto.
// Encoding this subset directly avoids importing or building BuildBuddy source.
func msg(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func str(n protowire.Number, s string) []byte { return msg(n, []byte(s)) }
func num(n protowire.Number, v int) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), uint64(v))
}
func join(bs ...[]byte) []byte {
	var out []byte
	for _, b := range bs {
		out = append(out, b...)
	}
	return out
}

type event struct{ id, wire []byte }

func ev(id []byte, field protowire.Number, payload []byte) event {
	return event{id, join(msg(1, id), msg(field, payload))}
}

type target struct {
	label                   string
	results                 []map[string]any
	status                  string
	runs                    int
	shards, attempts, total int
}
type model struct {
	events  []event
	targets []target
	log     string
	success bool
}

func generate(r *rand.Rand, iid string, boundary bool) model {
	m := model{success: true}
	m.events = append(m.events, ev(msg(15, str(1, "cfg")), 17, str(1, "k8-fastbuild")))
	targetCount := r.Intn(21)
	if boundary {
		targetCount = 17
	}
	for i := 0; i < targetCount; i++ {
		label := fmt.Sprintf("//pkg_%d:test_%d", r.Intn(100), i)
		runs := 1 + r.Intn(4)
		status := 1
		statusName := "PASSED"
		if r.Intn(3) == 0 && !boundary {
			status = 4
			statusName = "FAILED"
		}
		shards := 1 + r.Intn(4)
		attempts := 1
		if !boundary && i%3 == 0 {
			status, statusName = 2, "FLAKY"
			attempts = 2 + r.Intn(3)
		}
		if status == 4 {
			m.success = false
		}
		t := target{label: label, status: statusName, runs: runs, shards: shards, attempts: attempts, total: runs * shards * attempts}
		m.events = append(m.events, ev(msg(16, str(1, label)), 18, join(str(1, "go_test rule"), num(2, 1))))
		m.events = append(m.events, ev(msg(5, join(str(1, label), msg(3, str(1, "cfg")))), 8, num(1, 1)))
		for run := 1; run <= runs; run++ {
			for shard := 0; shard < shards; shard++ {
				for attempt := 1; attempt <= attempts; attempt++ {
					resultStatus, resultName := status, statusName
					if status == 2 {
						resultStatus, resultName = 4, "FAILED"
						if attempt == attempts {
							resultStatus, resultName = 1, "PASSED"
						}
					}
					details := fmt.Sprintf("seeded-%d-%d-λ", i, r.Intn(100000))
					cached := r.Intn(2) == 0
					id := msg(8, join(str(1, label), num(2, run), num(3, shard), num(4, attempt), msg(5, str(1, "cfg"))))
					payload := join(num(5, resultStatus), str(9, details))
					if cached {
						payload = join(payload, num(4, 1))
					}
					m.events = append(m.events, ev(id, 10, payload))
					result := map[string]any{"status": resultName, "statusDetails": details}
					if cached {
						result["cachedLocally"] = true
					}
					t.results = append(t.results, map[string]any{"id": map[string]any{"testResult": map[string]any{"label": label, "run": float64(run), "attempt": float64(attempt), "configuration": map[string]any{"id": "cfg"}}}, "testResult": result})
					if shard > 0 {
						object(object(t.results[len(t.results)-1]["id"])["testResult"])["shard"] = float64(shard)
					}
				}
			}
		}
		m.events = append(m.events, ev(msg(7, join(str(1, label), msg(2, str(1, "cfg")))), 9, join(num(5, status), num(1, t.total), num(10, runs), num(11, shards), num(15, attempts))))
		m.targets = append(m.targets, t)
	}
	// Plain short lines avoid terminal escape processing and line wrapping. Split
	// at arbitrary byte boundaries to exercise buffering independently of events.
	lineCount := 1 + r.Intn(150)
	if boundary {
		lineCount = 85000
	}
	var text strings.Builder
	for i := 0; i < lineCount; i++ {
		fmt.Fprintf(&text, "line %06d token %08x\n", i, r.Uint32())
	}
	m.log = text.String()
	for off, count := 0, 0; off < len(m.log); count++ {
		fragmentSize := 1 + r.Intn(170)
		if boundary {
			fragmentSize = 32768 + r.Intn(65536)
		}
		end := off + fragmentSize
		if end > len(m.log) {
			end = len(m.log)
		}
		payload := str(1, m.log[off:end])
		if r.Intn(2) == 0 {
			payload = str(2, m.log[off:end])
		}
		m.events = append(m.events, ev(msg(2, num(1, count)), 3, payload))
		off = end
	}
	exit := 0
	name := "SUCCESS"
	if !m.success {
		exit = 3
		name = "BUILD_FAILURE"
	}
	m.events = append(m.events, ev(msg(9, nil), 14, join(msg(3, join(str(1, name), num(2, exit))), num(2, 1700000001000))))
	started := ev(msg(3, nil), 5, join(str(1, iid), num(2, 1700000000000), str(3, "7.0.0"), str(4, "--color=no"), str(5, "test")))
	// Announce every child before it is posted, and close the event graph.
	for _, e := range m.events {
		started.wire = join(started.wire, msg(2, e.id))
	}
	m.events[len(m.events)-1].wire = join(m.events[len(m.events)-1].wire, num(20, 1))
	m.events = append([]event{started}, m.events...)
	return m
}

func publish(t *testing.T, c *harness.Clients, iid string, m model) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := c.BES.PublishBuildToolEventStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := &bes.StreamId{BuildId: iid, InvocationId: iid, Component: bes.StreamId_TOOL}
	// Read concurrently: bounded streams must not deadlock on ACK flow control.
	done := make(chan error, 1)
	go func() {
		next := int64(1)
		for {
			ack, e := s.Recv()
			if e == io.EOF {
				if next != int64(len(m.events)+2) {
					done <- fmt.Errorf("ACK count %d", next-1)
				} else {
					done <- nil
				}
				return
			}
			if e != nil {
				done <- e
				return
			}
			if ack.SequenceNumber != next || (ack.GetStreamId().GetBuildId() != iid || ack.GetStreamId().GetInvocationId() != iid || ack.GetStreamId().GetComponent() != bes.StreamId_TOOL) {
				done <- fmt.Errorf("unexpected ACK %v, want sequence %d", ack, next)
				return
			}
			next++
		}
	}()
	for i, e := range m.events {
		err = s.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: id, SequenceNumber: int64(i + 1), Event: &bes.BuildEvent{EventTime: timestamppb.New(time.Unix(1700000000, int64(i))), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: e.wire}}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	err = s.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: id, SequenceNumber: int64(len(m.events) + 1), Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_ComponentStreamFinished{ComponentStreamFinished: &bes.BuildEvent_BuildComponentStreamFinished{Type: bes.BuildEvent_BuildComponentStreamFinished_FINISHED}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

// Interrupt before BuildFinished, then wait for finalization before retrying the
// entire stream from sequence 1, as Bazel does for an unacknowledged stream.
func interrupt(t *testing.T, f *harness.Fixture, c *harness.Clients, iid string, m model, cut int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := c.BES.PublishBuildToolEventStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			_, err := s.Recv()
			if err != nil {
				done <- err
				return
			}
		}
	}()
	for i, e := range m.events[:cut] {
		if err := s.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: &bes.StreamId{BuildId: iid, InvocationId: iid, Component: bes.StreamId_TOOL}, SequenceNumber: int64(i + 1), Event: &bes.BuildEvent{EventTime: timestamppb.New(time.Unix(1700000000, int64(i))), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: e.wire}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CloseSend(); err != nil {
		t.Fatal(err)
	}
	t.Logf("interrupted at event %d: stream ended with %v", cut, <-done)
	deadline := time.Now().Add(15 * time.Second)
	for {
		res := rpc(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": iid}})
		invs := array(res["invocation"])
		if len(invs) == 1 && object(invs[0])["invocationStatus"] == "DISCONNECTED_INVOCATION_STATUS" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("interrupted invocation did not disconnect: %v", res)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func rpc(t *testing.T, f *harness.Fixture, method string, req map[string]any) map[string]any {
	t.Helper()
	req["requestContext"] = f.Web.RequestContext(f.GroupID)
	res, err := f.Web.RPC(method, req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func check(t *testing.T, f *harness.Fixture, iid string, m model) map[string]any {
	t.Helper()
	invs := array(rpc(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": iid}})["invocation"])
	if len(invs) != 1 {
		t.Fatalf("invocation count %d", len(invs))
	}
	inv := object(invs[0])
	if inv["invocationStatus"] != "COMPLETE_INVOCATION_STATUS" || inv["command"] != "test" || (inv["success"] == true) != m.success {
		t.Fatalf("invocation metadata: %v", inv)
	}
	// Verify invocation-level events have survived, with their original sequence.
	events := array(inv["event"])
	seen := map[string]bool{}
	for _, v := range events {
		e := object(v)
		b := object(e["buildEvent"])
		if payload, ok := b["started"]; ok {
			started := object(payload)
			if seen["started"] || fmt.Sprint(e["sequenceNumber"]) != "1" || started["uuid"] != iid || started["command"] != "test" || started["buildToolVersion"] != "7.0.0" || fmt.Sprint(started["startTimeMillis"]) != "1700000000000" {
				t.Fatalf("started event changed: %v", e)
			}
			seen["started"] = true
		}
		if payload, ok := b["finished"]; ok {
			finished := object(payload)
			name := "SUCCESS"
			if !m.success {
				name = "BUILD_FAILURE"
			}
			if seen["finished"] || fmt.Sprint(e["sequenceNumber"]) != fmt.Sprint(len(m.events)) || object(finished["exitCode"])["name"] != name || fmt.Sprint(finished["finishTimeMillis"]) != "1700000001000" || b["lastMessage"] != true {
				t.Fatalf("finished event changed: %v", e)
			}
			seen["finished"] = true
		}
		if payload, ok := b["configuration"]; ok {
			if seen["configuration"] || object(payload)["mnemonic"] != "k8-fastbuild" || object(object(b["id"])["configuration"])["id"] != "cfg" || fmt.Sprint(e["sequenceNumber"]) != "2" {
				t.Fatalf("configuration event changed: %v", e)
			}
			seen["configuration"] = true
		}
	}
	if !seen["started"] || !seen["finished"] || !seen["configuration"] {
		t.Fatalf("missing metadata events: %v", events)
	}
	snapshot := map[string]any{"success": inv["success"], "events": events}

	// Listing pages expose metadata, while single-target lookups expose details.
	// Verify no missing or extra labels, and total_count consistency on every page.
	for _, status := range []string{"PASSED", "FAILED", "FLAKY"} {
		expected := map[string]bool{}
		for _, target := range m.targets {
			if target.status == status {
				expected[target.label] = true
			}
		}
		seen := map[string]bool{}
		token := ""
		tokens := map[string]bool{}
		for page := 0; page < 100; page++ {
			res := rpc(t, f, "GetTarget", map[string]any{"invocationId": iid, "status": status, "pageToken": token})
			groups := array(res["targetGroups"])
			if len(groups) != 1 {
				t.Fatalf("listing groups: %v", res)
			}
			g := object(groups[0])
			count := fmt.Sprint(g["totalCount"])
			if count != fmt.Sprint(len(expected)) && !(len(expected) == 0 && g["totalCount"] == nil) {
				t.Fatalf("%s totalCount %s want %d", status, count, len(expected))
			}
			for _, v := range array(g["targets"]) {
				if object(v)["status"] != status || g["status"] != status {
					t.Fatalf("listing status disagrees: %v", g)
				}
				label, _ := object(object(v)["metadata"])["label"].(string)
				if !expected[label] || seen[label] {
					t.Fatalf("unexpected/duplicate listed target %q for %s", label, status)
				}
				seen[label] = true
			}
			next, _ := g["nextPageToken"].(string)
			if next == "" {
				break
			}
			if tokens[next] {
				t.Fatal("target page cycle")
			}
			tokens[next] = true
			token = next
			if page == 99 {
				t.Fatal("target pagination exceeded bound")
			}
		}
		if !reflect.DeepEqual(seen, expected) {
			t.Fatalf("%s listing got %v want %v", status, seen, expected)
		}
	}
	for _, want := range m.targets {
		res := rpc(t, f, "GetTarget", map[string]any{"invocationId": iid, "targetLabel": want.label})
		var got map[string]any
		for _, g := range array(res["targetGroups"]) {
			for _, v := range array(object(g)["targets"]) {
				candidate := object(v)
				if len(array(candidate["testResultEvents"])) > 0 {
					if got != nil {
						t.Fatalf("duplicate test target %s", want.label)
					}
					got = candidate
				}
			}
		}
		if got == nil {
			t.Fatalf("missing target %s: %v", want.label, res)
		}
		if object(got["metadata"])["label"] != want.label {
			t.Fatalf("target label: %v", got)
		}
		actual := array(got["testResultEvents"])
		if len(actual) != len(want.results) {
			t.Fatalf("%s result count %d want %d: %v", want.label, len(actual), len(want.results), got)
		}
		for i, w := range want.results {
			if !reflect.DeepEqual(actual[i], w) {
				t.Fatalf("%s result %d got %v want %v", want.label, i, actual[i], w)
			}
		}
		summary := object(got["testSummary"])
		if summary["overallStatus"] != want.status || summary["totalRunCount"] != float64(want.total) || summary["runCount"] != float64(want.runs) || summary["shardCount"] != float64(want.shards) || summary["attemptCount"] != float64(want.attempts) || got["status"] != want.status {
			t.Fatalf("summary %v", summary)
		}
		snapshot[want.label] = got
	}
	// Read from the last chunk backwards; empty ID means the last chunk.
	var chunks []string
	chunk := ""
	visited := map[string]bool{}
	for n := 0; n < 1000; n++ {
		if visited[chunk] {
			t.Fatalf("chunk cycle %q", chunk)
		}
		visited[chunk] = true
		res := rpc(t, f, "GetEventLogChunk", map[string]any{"invocationId": iid, "chunkId": chunk})
		encoded, _ := res["buffer"].(string)
		b, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, string(b))
		previous, _ := res["previousChunkId"].(string)
		if previous == "" {
			break
		}
		chunk = previous
		if n == 999 {
			t.Fatal("chunk traversal exceeded bound")
		}
	}
	if len(m.log) > 2_000_000 && len(chunks) < 2 {
		t.Fatalf("expected multiple log chunks, got %d", len(chunks))
	}
	var logs strings.Builder
	for i := len(chunks) - 1; i >= 0; i-- {
		logs.WriteString(chunks[i])
	}
	if logs.String() != m.log {
		gotLog := logs.String()
		at := 0
		for at < len(gotLog) && at < len(m.log) && gotLog[at] == m.log[at] {
			at++
		}
		t.Fatalf("logs differ at byte %d: got length %d want %d", at, len(gotLog), len(m.log))
	}
	snapshot["logs"] = logs.String()
	return snapshot
}

func TestRandomizedBESRoundTripAndReplay(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	if *iterations < 1 || *iterations > 200 {
		t.Fatal("iterations must be 1..200")
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--storage.chunk_file_size_bytes=1024"}}})
	c := f.Clients(harness.App)
	r := rand.New(rand.NewSource(*seed))
	deadline := time.Now().Add(4 * time.Minute)
	for i := 0; i < *iterations; i++ {
		if time.Now().After(deadline) {
			t.Fatalf("seed=%d exceeded runtime budget at iteration %d", *seed, i)
		}
		iid := uuid.NewString()
		m := generate(r, iid, i == 0)
		if !t.Run(fmt.Sprintf("seed_%d/iteration_%d", *seed, i), func(t *testing.T) {
			t.Logf("seed=%d iteration=%d invocation=%s events=%d", *seed, i, iid, len(m.events))
			if i%2 == 0 {
				interrupt(t, f, c, iid, m, 2+r.Intn(len(m.events)-3))
			}
			publish(t, c, iid, m)
			before := check(t, f, iid, m)
			publish(t, c, iid, m)
			after := check(t, f, iid, m)
			if !reflect.DeepEqual(before, after) {
				a, _ := json.Marshal(before)
				b, _ := json.Marshal(after)
				t.Fatalf("replay changed data: before %s after %s", a, b)
			}
		}) {
			return
		}
	}
}
