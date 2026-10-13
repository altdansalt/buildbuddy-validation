package history_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var seed = flag.Int64("history.seed", 314159, "Replay seed")
var iterations = flag.Int("history.iterations", 8, "Number of generated actions (1..40)")

// Encode the small BES envelope from the release's build_event_stream.proto.
// Keeping it wire-only avoids a dependency on BuildBuddy source/generated protos.
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func number(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func event(id, payload protowire.Number, body []byte, last bool) []byte {
	b := field(1, field(id, nil))
	b = append(b, field(payload, body)...)
	if last {
		b = append(b, number(20, 1)...)
	} else {
		b = append(b, field(2, field(9, nil))...)
	}
	return b
}

type observed struct {
	digest   *repb.Digest
	response *repb.ExecuteResponse
	target   string
}

func TestSeededExecutionHistory(t *testing.T) { runHistory(t, false) }

func TestTargetLabelFilter(t *testing.T) { runHistory(t, true) }

func runHistory(t *testing.T, targetFilter bool) {
	t.Logf("history.seed=%d history.iterations=%d", *seed, *iterations)
	require.True(t, *iterations > 0 && *iterations <= 40)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	invocation := uuid.NewString()
	bes, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	seq := int64(0)
	send := func(e *bespb.BuildEvent) {
		seq++
		require.NoError(t, bes.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: &bespb.StreamId{BuildId: invocation, InvocationId: invocation, Component: bespb.StreamId_TOOL}, SequenceNumber: seq, Event: e}}))
	}
	sendBazel := func(b []byte) {
		send(&bespb.BuildEvent{EventTime: timestamppb.Now(), Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: b}}})
	}
	started := field(1, []byte(invocation))
	started = append(started, number(2, uint64(time.Now().UnixMilli()))...)
	started = append(started, field(4, []byte("--remote_executor=grpc://app:1985"))...)
	started = append(started, field(5, []byte("test"))...)
	sendBazel(event(3, 5, started, false))
	root, err := c.UploadProto(ctx, "history", &repb.Directory{})
	require.NoError(t, err)
	rng := rand.New(rand.NewSource(*seed))
	expected := map[string]observed{}
	execute := func(d *repb.Digest, target string, skip bool) (string, *repb.ExecuteResponse) {
		md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: invocation, ActionId: target, TargetId: target, ActionMnemonic: "HistoryShell"})
		require.NoError(t, err)
		actionCtx := metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(md))
		stream, err := c.Execution.Execute(actionCtx, &repb.ExecuteRequest{InstanceName: "history", ActionDigest: d, SkipCacheLookup: skip, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		for {
			op, err := stream.Recv()
			require.NoError(t, err)
			if !op.Done {
				continue
			}
			require.Nil(t, op.GetError())
			result := &repb.ExecuteResponse{}
			require.NoError(t, op.GetResponse().UnmarshalTo(result))
			return op.Name, result
		}
	}
	for i := 0; i < *iterations; i++ {
		kind := i % 4
		exit := 0
		script := fmt.Sprintf("printf 'seed=%d case=%d value=%d\\n'; exit 0", *seed, i, rng.Int63())
		timeout := 10 * time.Second
		if kind == 1 {
			exit = 1 + rng.Intn(125)
			script = fmt.Sprintf("exit %d", exit)
		}
		if kind == 2 {
			script = "sleep 3"
			timeout = 300 * time.Millisecond
		}
		cmd, err := c.UploadProto(ctx, "history", &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
		require.NoError(t, err)
		d, err := c.UploadProto(ctx, "history", &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(timeout)})
		require.NoError(t, err)
		target := fmt.Sprintf("//history:case_%d", i)
		id, res := execute(d, target, true)
		if kind == 2 {
			require.EqualValues(t, 4, res.GetStatus().GetCode())
		} else {
			require.Zero(t, res.GetStatus().GetCode())
			require.EqualValues(t, exit, res.GetResult().GetExitCode())
		}
		expected[id] = observed{d, res, target}
		if kind == 3 {
			// A direct app AC hit schedules no execution and is intentionally absent
			// from history (execution_server.go execute's early cache-return path).
			_, hit := execute(d, target, false)
			require.True(t, hit.CachedResult)
			require.Equal(t, res.GetResult().GetExecutionMetadata().GetWorker(), hit.GetResult().GetExecutionMetadata().GetWorker())
		}
	}
	lookup := func(filters map[string]any) []map[string]any {
		filters["invocationId"] = invocation
		res, err := f.Web.RPC("GetExecution", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "executionLookup": filters, "inlineExecuteResponse": true})
		require.NoError(t, err)
		var rows []map[string]any
		if a, ok := res["execution"].([]any); ok {
			for _, v := range a {
				rows = append(rows, v.(map[string]any))
			}
		}
		return rows
	}
	check := func() {
		var rows []map[string]any
		deadline := time.Now().Add(15 * time.Second)
		for {
			rows = lookup(map[string]any{})
			if len(rows) == len(expected) || time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		require.Len(t, rows, len(expected), "seed=%d rows=%v", *seed, rows)
		seen := map[string]bool{}
		for _, row := range rows {
			id, _ := row["executionId"].(string)
			require.False(t, seen[id], "duplicate %s seed=%d", id, *seed)
			seen[id] = true
			want, ok := expected[id]
			require.True(t, ok, "unexpected %s seed=%d", id, *seed)
			decode := func(key string, m proto.Message) {
				// Auxiliary Any payloads are BuildBuddy-specific and are outside this oracle.
				stripAuxiliary(row[key])
				b, err := json.Marshal(row[key])
				require.NoError(t, err)
				require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, m))
			}
			d := &repb.Digest{}
			decode("actionDigest", d)
			require.True(t, proto.Equal(want.digest, d), "seed=%d digest %v", *seed, row)
			got := &repb.ExecuteResponse{}
			decode("executeResponse", got)
			require.Equal(t, want.response.GetStatus().GetCode(), got.GetStatus().GetCode())
			require.Equal(t, want.response.GetResult().GetExitCode(), got.GetResult().GetExitCode())
			statusMap, _ := row["status"].(map[string]any)
			code, _ := statusMap["code"].(float64)
			require.EqualValues(t, want.response.GetStatus().GetCode(), code)
			exitCode, _ := row["exitCode"].(float64)
			if code == 0 {
				require.EqualValues(t, want.response.GetResult().GetExitCode(), exitCode)
			}
			require.Equal(t, "COMPLETED", row["stage"])
			m := &repb.ExecutedActionMetadata{}
			decode("executedActionMetadata", m)
			require.NotEmpty(t, m.Worker)
			require.Equal(t, want.response.GetResult().GetExecutionMetadata().GetWorker(), m.Worker)
			stamps := []*timestamppb.Timestamp{m.WorkerStartTimestamp, m.InputFetchStartTimestamp, m.InputFetchCompletedTimestamp, m.ExecutionStartTimestamp, m.ExecutionCompletedTimestamp, m.OutputUploadStartTimestamp, m.OutputUploadCompletedTimestamp, m.WorkerCompletedTimestamp}
			var previous time.Time
			for _, stamp := range stamps {
				require.NotNil(t, stamp)
				require.NoError(t, stamp.CheckValid())
				now := stamp.AsTime()
				require.False(t, now.Before(previous), "seed=%d reversed timings: %v", *seed, m)
				previous = now
			}
			filters := []map[string]any{{"executionId": id}, {"actionDigestHash": d.Hash}}
			if targetFilter {
				filters = append(filters, map[string]any{"targetLabel": want.target})
			}
			for _, filter := range filters {
				filtered := lookup(filter)
				matches := map[string]bool{}
				for candidateID, candidate := range expected {
					match := true
					if v, ok := filter["executionId"]; ok {
						match = match && candidateID == v
					}
					if v, ok := filter["actionDigestHash"]; ok {
						match = match && candidate.digest.Hash == v
					}
					if v, ok := filter["targetLabel"]; ok {
						match = match && candidate.target == v
					}
					if match {
						matches[candidateID] = true
					}
				}
				var returnedIDs []string
				for _, r := range filtered {
					returnedIDs = append(returnedIDs, r["executionId"].(string))
				}
				t.Logf("seed=%d filter=%v expected_count=%d actual_count=%d execution_ids=%v", *seed, filter, len(matches), len(filtered), returnedIDs)
				require.Equal(t, len(matches), len(filtered), "seed=%d filter=%v", *seed, filter)
				for _, returnedID := range returnedIDs {
					require.True(t, matches[returnedID], "unexpected filtered execution %s", returnedID)
					delete(matches, returnedID)
				}
				require.Empty(t, matches)
			}
		}
	}
	check()
	finished := field(3, field(1, []byte("SUCCESS")))
	finished = append(finished, number(2, uint64(time.Now().UnixMilli()))...)
	sendBazel(event(9, 14, finished, true))
	send(&bespb.BuildEvent{EventTime: timestamppb.Now(), Event: &bespb.BuildEvent_ComponentStreamFinished{ComponentStreamFinished: &bespb.BuildEvent_BuildComponentStreamFinished{}}})
	require.NoError(t, bes.CloseSend())
	for ackSeq := int64(1); ackSeq <= seq; ackSeq++ {
		ack, err := bes.Recv()
		require.NoError(t, err)
		require.Equal(t, ackSeq, ack.SequenceNumber)
	}
	_, err = bes.Recv()
	require.Equal(t, io.EOF, err)
	check()
}

func stripAuxiliary(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "auxiliaryMetadata")
		for _, child := range x {
			stripAuxiliary(child)
		}
	case []any:
		for _, child := range x {
			stripAuxiliary(child)
		}
	}
}
