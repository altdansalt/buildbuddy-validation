package bes_lifecycle_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type model struct {
	id, state string
	success   bool
	duration  string
}

func checkModel(t *testing.T, f *harness.Fixture, c *harness.Clients, m model) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	convergenceStart := time.Now()
	req := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": m.id}}
	// Poll the state rather than assuming Send is a persistence barrier.
	require.Eventually(t, func() bool {
		r, e := f.Web.RPC("GetInvocation", req)
		if e != nil {
			return false
		}
		a, _ := r["invocation"].([]any)
		return len(a) == 1 && a[0].(map[string]any)["invocationStatus"] == m.state && a[0].(map[string]any)["user"] == "fixture-user"
	}, 20*time.Second, 100*time.Millisecond)
	var ui, direct, search, gs, rest, ag, commitRest, commitGRPC map[string]any
	var err error
	var code, commitCode int
	var surfaces map[string]map[string]any
	// Final cache-stat collection can update the DB asynchronously after ACKs.
	// Retain a converged snapshot so a concurrent legitimate timestamp update
	// does not look like a permanent cross-surface inconsistency.
	require.Eventually(t, func() bool {
		ui = rpc(t, f, "GetInvocation", req)
		direct, err = grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetInvocation", req)
		require.NoError(t, err)
		query := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "query": map[string]any{"groupId": f.GroupID, "user": "fixture-user", "host": "fixture-host", "repoUrl": "https://example.invalid/bes-lifecycle", "commitSha": "0123456789abcdef", "branchName": "validation"}}
		search = rpc(t, f, "SearchInvocation", query)
		gs, err = grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "SearchInvocation", query)
		require.NoError(t, err)
		ar := map[string]any{"selector": map[string]any{"invocationId": m.id}, "includeMetadata": true}
		code, rest = api(t, f, "GetInvocation", ar)
		require.Equal(t, 200, code)
		ag, err = grpcRead(t, ctx, c, "api.v1.ApiService", "GetInvocation", ar)
		require.NoError(t, err)
		commitReq := map[string]any{"selector": map[string]any{"commitSha": "0123456789abcdef"}, "includeMetadata": true}
		commitCode, commitRest = api(t, f, "GetInvocation", commitReq)
		require.Equal(t, 200, commitCode)
		commitGRPC, err = grpcRead(t, ctx, c, "api.v1.ApiService", "GetInvocation", commitReq)
		require.NoError(t, err)
		surfaces = map[string]map[string]any{"ui": ui, "grpc": direct, "search-ui": search, "search-grpc": gs, "rest": rest, "api-grpc": ag, "commit-rest": commitRest, "commit-grpc": commitGRPC}
		var stamp any
		for _, r := range surfaces {
			rows, _ := r["invocation"].([]any)
			if len(rows) != 1 {
				return false
			}
			inv := rows[0].(map[string]any)
			if inv["invocationStatus"] != m.state {
				return false
			}
			if stamp == nil {
				stamp = inv["updatedAtUsec"]
			} else if stamp != inv["updatedAtUsec"] {
				return false
			}
		}
		return true
	}, 20*time.Second, 100*time.Millisecond)
	t.Logf("%s %s reader convergence observed within %s", m.id, m.state, time.Since(convergenceStart))
	ownerReq := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": m.id}
	// This owner lookup is server-admin-only, unlike the org-authorized readers.
	_, err = f.Web.RPC("GetInvocationOwner", ownerReq)
	require.ErrorContains(t, err, "PermissionDenied")
	_, err = grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetInvocationOwner", ownerReq)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	for name, r := range surfaces {
		a := objects(t, r, "invocation")
		require.Len(t, a, 1, name)
		inv := a[0].(map[string]any)
		if name == "rest" || name == "api-grpc" || name == "commit-rest" || name == "commit-grpc" {
			require.Equal(t, m.id, inv["id"].(map[string]any)["invocationId"], name)
			metadata := map[string]string{}
			for _, item := range objects(t, inv, "buildMetadata") {
				v := item.(map[string]any)
				metadata[v["key"].(string)] = v["value"].(string)
			}
			require.Equal(t, map[string]string{"ROLE": "CI", "REPO_URL": "https://example.invalid/bes-lifecycle", "COMMIT_SHA": "0123456789abcdef", "BRANCH_NAME": "validation"}, metadata, name+":metadata")
			require.Equal(t, []any{map[string]any{"key": "BUILD_USER", "value": "fixture-user"}, map[string]any{"key": "BUILD_HOST", "value": "fixture-host"}}, inv["workspaceStatus"], name+":workspace")
		} else {
			require.Equal(t, m.id, inv["invocationId"], name)
		}
		require.Equal(t, m.state, inv["invocationStatus"], name)
		require.Equal(t, m.success, inv["success"] == true, name)
		for k, v := range map[string]string{"command": "test", "user": "fixture-user", "host": "fixture-host", "repoUrl": "https://example.invalid/bes-lifecycle", "commitSha": "0123456789abcdef", "branchName": "validation"} {
			require.Equal(t, v, inv[k], name+":"+k)
		}
		base := objects(t, ui, "invocation")[0].(map[string]any)
		require.NotEmpty(t, inv["createdAtUsec"], name)
		require.Equal(t, base["createdAtUsec"], inv["createdAtUsec"], name+":created")
		require.NotEmpty(t, inv["updatedAtUsec"], name)
		if m.state == "COMPLETE_INVOCATION_STATUS" {
			require.Equal(t, base["updatedAtUsec"], inv["updatedAtUsec"], name+":updated")
			require.Equal(t, map[bool]string{true: "SUCCESS", false: "BUILD_FAILURE"}[m.success], inv["bazelExitCode"], name)
		}
		require.Equal(t, "CI", inv["role"], name)
		if m.duration != "" {
			require.Equal(t, m.duration, inv["durationUsec"], name)
		}
	}
	statReq := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "aggregationType": "USER_AGGREGATION_TYPE", "query": map[string]any{"user": "fixture-user"}}
	sr := rpc(t, f, "GetInvocationStat", statReq)
	sg, e := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetInvocationStat", statReq)
	require.NoError(t, e)
	require.Equal(t, sr["invocationStat"], sg["invocationStat"])
	stat := objects(t, sr, "invocationStat")
	require.Len(t, stat, 1)
	require.Equal(t, "1", stat[0].(map[string]any)["totalNumBuilds"])
	trReq := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "query": map[string]any{"user": "fixture-user"}}
	tr := rpc(t, f, "GetTrend", trReq)
	tg, e := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetTrend", trReq)
	require.NoError(t, e)
	require.Equal(t, tr["trendStat"], tg["trendStat"])
	trends := objects(t, tr, "trendStat")
	require.Len(t, trends, 1)
	require.Equal(t, "1", trends[0].(map[string]any)["totalNumBuilds"])
	if m.state == "PARTIAL_INVOCATION_STATUS" {
		require.Equal(t, "1", trends[0].(map[string]any)["otherBuilds"])
	}

}

func TestLifecycleModel(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprintf("success=%v", success), func(t *testing.T) { runLifecycle(t, success) })
	}
}
func runLifecycle(t *testing.T, success bool) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	c := f.Clients(harness.App)
	proxy := f.Clients(harness.CacheProxy)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	id := uuid.NewString()
	sid := &bes.StreamId{BuildId: id, InvocationId: id, Component: bes.StreamId_TOOL}
	// Lifecycle notifications are accepted no-ops in a standalone app (no forwarding peers).
	lifecycle := &bes.PublishLifecycleEventRequest{ProjectId: "fixture", BuildEvent: &bes.OrderedBuildEvent{StreamId: &bes.StreamId{BuildId: id, Component: bes.StreamId_CONTROLLER}, SequenceNumber: 1, Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BuildEnqueued_{BuildEnqueued: &bes.BuildEvent_BuildEnqueued{}}}}}
	for i := 0; i < 2; i++ {
		_, err := c.BES.PublishLifecycleEvent(ctx, lifecycle)
		require.NoError(t, err)
		checkAbsent(t, f, c, id)
	}
	_, err := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
	require.ErrorContains(t, err, "NotFound")
	// Cache proxy registers cache services, not BES or invocation projections.
	_, err = proxy.BES.PublishLifecycleEvent(ctx, lifecycle)
	require.Equal(t, codes.Unimplemented, status.Code(err))
	startedLife := map[string]any{"projectId": "fixture", "buildEvent": map[string]any{"streamId": map[string]any{"buildId": id, "invocationId": id, "component": "CONTROLLER"}, "sequenceNumber": "2", "event": map[string]any{"eventTime": time.Now().UTC().Format(time.RFC3339Nano), "invocationAttemptStarted": map[string]any{"attemptNumber": "1"}}}}
	_, err = grpcRead(t, ctx, c, "google.devtools.build.v1.PublishBuildEvent", "PublishLifecycleEvent", startedLife)
	require.NoError(t, err)

	checkAbsent(t, f, c, id)
	s, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	requests := fixtureRequests(t, id, success)
	ps, proxyErr := proxy.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, proxyErr)
	proxyErr = ps.Send(requests[0])
	if proxyErr != nil {
		require.ErrorIs(t, proxyErr, io.EOF)
	}
	_, proxyErr = ps.Recv()
	require.Equal(t, codes.Unimplemented, status.Code(proxyErr))
	checkProxyInvocationBoundary(t, ctx, c, proxy, id)
	for _, r := range requests[:4] {
		require.NoError(t, s.Send(r))
	}
	checkModel(t, f, c, model{id: id, state: "PARTIAL_INVOCATION_STATUS"})
	for _, r := range requests[4:] {
		require.NoError(t, s.Send(r))
	}
	require.NoError(t, s.CloseSend())
	for i := range requests {
		ack, e := s.Recv()
		require.NoError(t, e)
		require.Equal(t, int64(i+1), ack.SequenceNumber)
		require.True(t, proto.Equal(sid, ack.StreamId))
	}
	_, err = s.Recv()
	require.Equal(t, io.EOF, err)
	checkModel(t, f, c, model{id: id, state: "COMPLETE_INVOCATION_STATUS", success: success, duration: "1500000"})
	checkGraph(t, f, c, requests)
	for i, kind := range []string{"invocationAttemptFinished", "buildFinished"} {
		statusKey := "invocationStatus"
		if kind == "buildFinished" {
			statusKey = "status"
		}
		req := map[string]any{"projectId": "fixture", "buildEvent": map[string]any{"streamId": map[string]any{"buildId": id, "invocationId": id, "component": "CONTROLLER"}, "sequenceNumber": fmt.Sprint(i + 3), "event": map[string]any{"eventTime": time.Now().UTC().Format(time.RFC3339Nano), kind: map[string]any{statusKey: map[string]any{"result": map[bool]string{true: "COMMAND_SUCCEEDED", false: "COMMAND_FAILED"}[success]}}}}}
		_, err = grpcRead(t, ctx, c, "google.devtools.build.v1.PublishBuildEvent", "PublishLifecycleEvent", req)
		require.NoError(t, err)
		checkModel(t, f, c, model{id: id, state: "COMPLETE_INVOCATION_STATUS", success: success, duration: "1500000"})
		checkGraph(t, f, c, requests)
		checkCLI(t, ctx, f, id, "state-machine console output\n")
	}

	beforeReplay := rpc(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})
	frozen := objects(t, beforeReplay, "invocation")[0].(map[string]any)
	// A valid complete replay must neither duplicate metadata nor console output.
	s, err = c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	for _, r := range requests {
		require.NoError(t, s.Send(r))
	}
	require.NoError(t, s.CloseSend())
	for i := range requests {
		ack, e := s.Recv()
		require.NoError(t, e)
		require.Equal(t, int64(i+1), ack.SequenceNumber)
		require.True(t, proto.Equal(sid, ack.StreamId))
	}
	_, err = s.Recv()
	require.Equal(t, io.EOF, err)
	checkModel(t, f, c, model{id: id, state: "COMPLETE_INVOCATION_STATUS", success: success, duration: "1500000"})
	afterReplay := rpc(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})
	replayed := objects(t, afterReplay, "invocation")[0].(map[string]any)
	for _, key := range []string{"createdAtUsec", "updatedAtUsec"} {
		require.Equal(t, frozen[key], replayed[key], "replay must preserve "+key)
	}
	logs := rpc(t, f, "GetEventLogChunk", map[string]any{"invocationId": id, "chunkId": "0000"})
	b, err := base64.StdEncoding.DecodeString(logs["buffer"].(string))
	require.NoError(t, err)
	require.Equal(t, "state-machine console output\n", string(b))
	code, al := api(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": id}})
	require.Equal(t, 200, code)
	require.Equal(t, string(b), al["log"].(map[string]any)["contents"])
	gl, err := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetEventLogChunk", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id, "chunkId": "0000"})
	require.NoError(t, err)
	require.Equal(t, logs["buffer"], gl["buffer"])
	pl, err := grpcRead(t, ctx, c, "api.v1.ApiService", "GetLog", map[string]any{"selector": map[string]any{"invocationId": id}})
	require.NoError(t, err)
	require.Equal(t, al["log"], pl["log"])

	checkCLI(t, ctx, f, id, string(b))
	checkGraph(t, f, c, requests)

	statReq := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "aggregationType": "USER_AGGREGATION_TYPE", "query": map[string]any{"user": "fixture-user"}}
	stats := rpc(t, f, "GetInvocationStat", statReq)
	st := objects(t, stats, "invocationStat")
	require.Len(t, st, 1)
	require.Equal(t, "1", st[0].(map[string]any)["totalNumBuilds"])
	if success {
		require.Equal(t, "1", st[0].(map[string]any)["totalNumSucessfulBuilds"])
	} else {
		require.Equal(t, "1", st[0].(map[string]any)["totalNumFailingBuilds"])
	}
	gr, err := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetInvocationStat", statReq)
	require.NoError(t, err)
	require.Equal(t, stats["invocationStat"], gr["invocationStat"])
	trendReq := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "query": map[string]any{"user": "fixture-user"}}
	tr := rpc(t, f, "GetTrend", trendReq)
	require.Len(t, objects(t, tr, "trendStat"), 1)
	trend := objects(t, tr, "trendStat")[0].(map[string]any)
	require.Equal(t, "1", trend["totalNumBuilds"])
	require.Equal(t, "1500000", trend["totalBuildTimeUsec"])
	if success {
		require.Equal(t, "1", trend["successfulBuilds"])
	} else {
		require.Equal(t, "1", trend["failedBuilds"])
	}
	gt, err := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetTrend", trendReq)
	require.NoError(t, err)
	require.Equal(t, tr["trendStat"], gt["trendStat"])
	_, err = grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetInvocationFilterSuggestions", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
	require.Equal(t, codes.Unimplemented, status.Code(err))
	_, err = f.Web.RPC("GetInvocationFilterSuggestions", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
	require.ErrorContains(t, err, "Unimplemented")
	// Audit missed UI downloads before deletion (after complete replay).
	for _, artifact := range []string{"raw_json", "buildlog"} {
		code, body := auditDownload(t, ctx, f, id, artifact)
		require.Equal(t, 200, code)
		if artifact == "buildlog" {
			require.Equal(t, "state-machine console output\n", string(body))
		} else {
			var events []map[string]any
			require.NoError(t, json.Unmarshal(body, &events))
			require.Len(t, events, 5, "raw JSON must contain exactly the published Bazel events after replay")
			require.Equal(t, id, events[0]["started"].(map[string]any)["uuid"])
			// The released handler moves stdout into chunked logs and clears it from stored events.
			require.Contains(t, events[3], "progress")
			require.Empty(t, events[3]["progress"].(map[string]any)["stdout"])
			require.Equal(t, map[bool]string{true: "SUCCESS", false: "BUILD_FAILURE"}[success], events[4]["finished"].(map[string]any)["exitCode"].(map[string]any)["name"])
		}
		t.Logf("AUDIT download %s: HTTP %d, %d bytes, expected contents", artifact, code, len(body))
	}
	// Deletion is a valid invocation mutation exposed by BuildBuddyService.
	del := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id}
	_, err = grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "DeleteInvocation", del)
	require.NoError(t, err)
	checkAbsent(t, f, c, id)
	out, cliErr := cliOutput(t, ctx, f, id)
	require.Error(t, cliErr)
	require.Contains(t, string(out), "NotFound")
	require.NotContains(t, string(out), "state-machine console output")
	lookup := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}}
	_, err = f.Web.RPC("GetInvocation", lookup)
	require.ErrorContains(t, err, "NotFound")
	_, err = grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetInvocation", lookup)
	require.Equal(t, codes.NotFound, status.Code(err))
	for _, method := range []string{"SearchInvocation", "GetInvocationStat", "GetTrend"} {
		request := trendReq
		key := "trendStat"
		if method == "SearchInvocation" {
			request = map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "query": map[string]any{"groupId": f.GroupID, "user": "fixture-user"}}
			key = "invocation"
		}
		if method == "GetInvocationStat" {
			request = statReq
			key = "invocationStat"
		}
		r := rpc(t, f, method, request)
		require.Empty(t, objects(t, r, key), method)
		g, e := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", method, request)
		require.NoError(t, e)
		require.Empty(t, objects(t, g, key), method)
	}
	ar := map[string]any{"selector": map[string]any{"invocationId": id}}
	code, rest := api(t, f, "GetInvocation", ar)
	require.Equal(t, 200, code)
	require.Empty(t, objects(t, rest, "invocation"))
	ag, err := grpcRead(t, ctx, c, "api.v1.ApiService", "GetInvocation", ar)
	require.NoError(t, err)
	require.Empty(t, objects(t, ag, "invocation"))
	deletedDownloads := map[string]int{}
	for _, artifact := range []string{"raw_json", "buildlog"} {
		code, body := auditDownload(t, ctx, f, id, artifact)
		deletedDownloads[artifact] = code
		t.Logf("AUDIT deleted download %s: HTTP %d body=%q", artifact, code, body)
	}
	_, err = f.Web.RPC("GetEventLogChunk", map[string]any{"invocationId": id, "chunkId": "0000"})
	require.ErrorContains(t, err, "NotFound")
	for _, serviceMethod := range [][2]string{{"buildbuddy.service.BuildBuddyService", "GetEventLogChunk"}, {"api.v1.ApiService", "GetLog"}} {
		req := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id, "chunkId": "0000"}
		if serviceMethod[1] == "GetLog" {
			req = map[string]any{"selector": map[string]any{"invocationId": id}}
		}
		_, err = grpcRead(t, ctx, c, serviceMethod[0], serviceMethod[1], req)
		require.Equal(t, codes.NotFound, status.Code(err))
	}
	code, result := api(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": id}})
	require.NotEqual(t, 200, code)
	require.Contains(t, result["error"], "NotFound")
	t.Logf("AUDIT deleted logs: UI/gRPC NotFound; REST HTTP %d body=%v", code, result)
	// Missing-resource download status is asserted in the isolated bug target.
}
