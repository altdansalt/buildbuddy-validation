package invocations_api_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func started(id string) []byte {
	return event(3, 5, nil, msg(str(1, id), num(2, time.Now().UnixMilli()), str(3, "8.0.0"), str(4, "--color=no"), str(5, "test")))
}
func finished() []byte {
	return msg(event(9, 14, nil, msg(field(3, msg(str(1, "SUCCESS"))), num(2, time.Now().UnixMilli()))), num(20, 1))
}
func sendEvent(t *testing.T, s bes.PublishBuildEvent_PublishBuildToolEventStreamClient, id string, seq int64, b []byte) {
	t.Helper()
	require.NoError(t, s.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: &bes.StreamId{BuildId: id, InvocationId: id, Component: bes.StreamId_TOOL}, SequenceNumber: seq, Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: b}}}}}))
}
func closeStream(t *testing.T, s bes.PublishBuildEvent_PublishBuildToolEventStreamClient, first, count int) {
	t.Helper()
	require.NoError(t, s.CloseSend())
	for i := 0; i < count; i++ {
		a, err := s.Recv()
		require.NoError(t, err)
		require.Equal(t, int64(first+i), a.SequenceNumber)
	}
	_, err := s.Recv()
	require.Equal(t, io.EOF, err)
}
func invocationStatus(t *testing.T, f *harness.Fixture, id string) string {
	t.Helper()
	r := rpc(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})
	invs := objects(t, r, "invocation")
	require.Len(t, invs, 1)
	return invs[0].(map[string]any)["invocationStatus"].(string)
}

func TestInterruptedAndRetriedUploads(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", abort), func(t *testing.T) {
			id := uuid.NewString()
			attemptCtx, stop := context.WithCancel(ctx)
			defer stop()
			s, err := f.Clients(harness.App).BES.PublishBuildToolEventStream(attemptCtx)
			require.NoError(t, err)
			sendEvent(t, s, id, 1, started(id))
			sendEvent(t, s, id, 2, event(2, 3, num(1, 0), str(1, "abandoned attempt\n")))
			// Observe server-side processing before simulating transport loss.
			require.Eventually(t, func() bool {
				r, e := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
				return e == nil && len(r["invocation"].([]any)) == 1
			}, 30*time.Second, 100*time.Millisecond)
			require.Equal(t, "PARTIAL_INVOCATION_STATUS", invocationStatus(t, f, id))
			if abort {
				stop()
				_, err = s.Recv()
				require.Error(t, err)
			} else {
				closeStream(t, s, 1, 2)
			}
			require.Eventually(t, func() bool {
				r, e := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
				if e != nil {
					return false
				}
				return r["invocation"].([]any)[0].(map[string]any)["invocationStatus"] == "DISCONNECTED_INVOCATION_STATUS"
			}, 30*time.Second, 100*time.Millisecond)
			s, err = f.Clients(harness.App).BES.PublishBuildToolEventStream(ctx)
			require.NoError(t, err)
			sendEvent(t, s, id, 1, started(id))
			sendEvent(t, s, id, 2, event(2, 3, num(1, 0), str(1, "successful retry\n")))
			sendEvent(t, s, id, 3, finished())
			closeStream(t, s, 1, 3)
			require.Equal(t, "COMPLETE_INVOCATION_STATUS", invocationStatus(t, f, id))
			code, r := api(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": id}})
			require.Equal(t, 200, code)
			require.Equal(t, "successful retry\n", r["log"].(map[string]any)["contents"])
			// A suffix retry represents lost ACKs after finalization. It must not mutate the invocation.
			s, err = f.Clients(harness.App).BES.PublishBuildToolEventStream(ctx)
			require.NoError(t, err)
			sendEvent(t, s, id, 3, finished())
			closeStream(t, s, 3, 1)
			require.Equal(t, "COMPLETE_INVOCATION_STATUS", invocationStatus(t, f, id))
			code, r = api(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": id}})
			require.Equal(t, 200, code)
			require.Equal(t, "successful retry\n", r["log"].(map[string]any)["contents"])
		})
	}
}

func TestStreamingReadsAndLargeTargetPagination(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	id := uuid.NewString()
	s, err := f.Clients(harness.App).BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	sendEvent(t, s, id, 1, started(id))
	require.Eventually(t, func() bool {
		_, e := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
		return e == nil
	}, 30*time.Second, 100*time.Millisecond)
	const count = 257
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 12; i++ {
				for _, method := range []string{"GetInvocation", "GetTarget", "SearchInvocation"} {
					req := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)}
					switch method {
					case "GetInvocation":
						req["lookup"] = map[string]any{"invocationId": id}
					case "GetTarget":
						req["invocationId"] = id
					case "SearchInvocation":
						req["query"] = map[string]any{"groupId": f.GroupID}
					}
					r, e := f.Web.RPC(method, req)
					if e != nil {
						errs <- fmt.Errorf("%s: %w", method, e)
						return
					}
					if method == "GetInvocation" {
						a, ok := r["invocation"].([]any)
						if !ok || len(a) != 1 || a[0].(map[string]any)["invocationStatus"] != "PARTIAL_INVOCATION_STATUS" {
							errs <- fmt.Errorf("unexpected streaming invocation: %v", r)
							return
						}
					}
				}
			}
		}()
	}
	seq := int64(2)
	for i := 0; i < count; i++ {
		label := fmt.Sprintf("//large:test_%04d", i)
		for _, b := range [][]byte{event(4, 6, str(1, label), nil), event(16, 18, str(1, label), msg(str(1, "sh_test rule"), num(2, 1))), event(5, 8, str(1, label), num(1, 1)), event(7, 9, str(1, label), msg(num(5, 1), num(1, 1)))} {
			sendEvent(t, s, id, seq, b)
			seq++
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	sendEvent(t, s, id, seq, finished())
	closeStream(t, s, 1, int(seq))
	require.Equal(t, "COMPLETE_INVOCATION_STATUS", invocationStatus(t, f, id))
	seen := map[string]bool{}
	token := ""
	pages := 0
	for {
		r := rpc(t, f, "GetTarget", map[string]any{"invocationId": id, "status": "PASSED", "pageToken": token})
		groups := objects(t, r, "targetGroups")
		require.Len(t, groups, 1)
		g := groups[0].(map[string]any)
		targets := objects(t, g, "targets")
		require.NotEmpty(t, targets)
		require.LessOrEqual(t, len(targets), 12)
		require.Equal(t, fmt.Sprint(count), g["totalCount"])
		for _, v := range targets {
			label := v.(map[string]any)["metadata"].(map[string]any)["label"].(string)
			require.False(t, seen[label], "duplicate %s", label)
			seen[label] = true
		}
		pages++
		next, _ := g["nextPageToken"].(string)
		if next == "" {
			break
		}
		require.NotEqual(t, token, next)
		require.Less(t, pages, 30)
		token = next
	}
	require.Greater(t, pages, 1)
	require.Len(t, seen, count)
	for i := 0; i < count; i++ {
		require.True(t, seen[fmt.Sprintf("//large:test_%04d", i)])
	}
	code, r := api(t, f, "GetTarget", map[string]any{"selector": map[string]any{"invocationId": id}})
	require.Equal(t, 200, code)
	require.Len(t, objects(t, r, "target"), count)
	require.Empty(t, r["nextPageToken"])
}
