package bes_lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/grpcreflect"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTargetActionArtifacts(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	c := f.Clients(harness.App)
	proxy := f.Clients(harness.CacheProxy)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	id := uuid.NewString()
	label := "//fixture:binary"
	content := []byte("fixture target artifact\n")
	digest, e := proxy.Upload(ctx, "", content)
	require.NoError(t, e)
	for _, reader := range []*harness.Clients{c, proxy} {
		b, e := reader.Read(ctx, "", digest)
		require.NoError(t, e)
		require.Equal(t, content, b)
	}
	uri := fmt.Sprintf("bytestream://app:1985/blobs/%s/%d", digest.Hash, digest.SizeBytes)
	file := map[string]any{"name": "output.txt", "uri": uri}
	configuredID := map[string]any{"targetConfigured": map[string]any{"label": label}}
	completedID := map[string]any{"targetCompleted": map[string]any{"label": label, "configuration": map[string]any{"id": "none"}}}
	actionID := map[string]any{"actionCompleted": map[string]any{"label": label, "configuration": map[string]any{"id": "none"}, "primaryOutput": "output.txt"}}
	rc := grpcreflect.NewClientAuto(ctx, c.Conn)
	defer rc.Reset()
	desc, e := rc.ResolveMessage("build_event_stream.BuildEvent")
	require.NoError(t, e)
	encode := func(v map[string]any) []byte {
		b, e := json.Marshal(v)
		require.NoError(t, e)
		m := dynamic.NewMessage(desc)
		require.NoError(t, m.UnmarshalJSON(b))
		b, e = m.Marshal()
		require.NoError(t, e)
		return b
	}
	requests := fixtureRequests(t, id, true)
	m := dynamic.NewMessage(desc)
	require.NoError(t, m.Unmarshal(requests[0].OrderedBuildEvent.Event.GetBazelEvent().Value))
	b, e := m.MarshalJSON()
	require.NoError(t, e)
	var started map[string]any
	require.NoError(t, json.Unmarshal(b, &started))
	started["children"] = append(started["children"].([]any), configuredID)
	requests[0].OrderedBuildEvent.Event.GetBazelEvent().Value = encode(started)
	extra := []map[string]any{
		{"id": configuredID, "configured": map[string]any{"targetKind": "sh_binary rule", "tag": []string{"fixture"}}, "children": []any{completedID}},
		{"id": completedID, "completed": map[string]any{"success": true, "outputGroup": []any{map[string]any{"name": "default", "fileSets": []any{map[string]any{"id": "fixture-files"}}}}}, "children": []any{actionID, map[string]any{"namedSet": map[string]any{"id": "fixture-files"}}}},
		{"id": map[string]any{"namedSet": map[string]any{"id": "fixture-files"}}, "namedSetOfFiles": map[string]any{"files": []any{file}}},
		{"id": actionID, "action": map[string]any{"success": true, "type": "Fixture", "stdout": file, "primaryOutput": file}},
	}
	all := append([]*bes.PublishBuildToolEventStreamRequest{}, requests[:2]...)
	for _, event := range extra {
		all = append(all, &bes.PublishBuildToolEventStreamRequest{ProjectId: "fixture", OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: requests[0].OrderedBuildEvent.StreamId, Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: encode(event)}}}}})
	}
	all = append(all, requests[2:]...)
	stream, e := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, e)
	for i, r := range all {
		r.OrderedBuildEvent.SequenceNumber = int64(i + 1)
		require.NoError(t, stream.Send(r))
	}
	require.NoError(t, stream.CloseSend())
	for i := range all {
		ack, e := stream.Recv()
		require.NoError(t, e)
		require.Equal(t, int64(i+1), ack.SequenceNumber)
	}
	_, e = stream.Recv()
	require.Equal(t, io.EOF, e)
	checkModel(t, f, c, model{id: id, state: "COMPLETE_INVOCATION_STATUS", success: true, duration: "1500000"})
	// Both UI target listings (including artifact-filter mode) and public API
	// selectors have independently modeled values, not just reader equality.
	for pass := 0; pass < 2; pass++ {
		for _, selector := range []map[string]any{{"invocationId": id}, {"invocationId": id, "label": label}, {"invocationId": id, "tag": "fixture"}} {
			req := map[string]any{"selector": selector}
			code, r := api(t, f, "GetTarget", req)
			require.Equal(t, 200, code)
			g, e := grpcRead(t, ctx, c, "api.v1.ApiService", "GetTarget", req)
			require.NoError(t, e)
			for _, response := range []map[string]any{r, g} {
				rows := objects(t, response, "target")
				require.Len(t, rows, 1)
				target := rows[0].(map[string]any)
				require.Equal(t, label, target["label"])
				require.Equal(t, "BUILT", target["status"])
				require.Equal(t, "sh_binary", target["ruleType"])
				require.Equal(t, "sh", target["language"])
				require.Equal(t, []any{"fixture"}, target["tag"])
				require.Empty(t, response["nextPageToken"])
			}
		}
		ar := map[string]any{"selector": map[string]any{"invocationId": id, "targetLabel": label}}
		code, r := api(t, f, "GetAction", ar)
		require.Equal(t, 200, code)
		g, e := grpcRead(t, ctx, c, "api.v1.ApiService", "GetAction", ar)
		require.NoError(t, e)
		require.Equal(t, r["action"], g["action"])
		rows := objects(t, r, "action")
		require.Len(t, rows, 2)
		var files int
		for _, row := range rows {
			action := row.(map[string]any)
			require.Equal(t, label, action["targetLabel"])
			require.Equal(t, "none", action["id"].(map[string]any)["configurationId"])
			for _, item := range objects(t, action, "file") {
				f := item.(map[string]any)
				require.Equal(t, uri, f["uri"])
				require.Equal(t, digest.Hash, f["hash"])
				files++
			}
		}
		require.Equal(t, 1, files)
		for _, req := range []map[string]any{{"invocationId": id, "targetLabel": label}, {"invocationId": id, "status": "STATUS_UNSPECIFIED", "filter": "output.txt"}} {
			r := rpc(t, f, "GetTarget", req)
			req["requestContext"] = f.Web.RequestContext(f.GroupID)
			g, e := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "GetTarget", req)
			require.NoError(t, e)
			require.Equal(t, r["targetGroups"], g["targetGroups"])
			groups := objects(t, r, "targetGroups")
			require.Len(t, groups, 1)
			targets := objects(t, groups[0].(map[string]any), "targets")
			require.Len(t, targets, 1)
			target := targets[0].(map[string]any)
			require.Equal(t, label, target["metadata"].(map[string]any)["label"])
			require.Len(t, objects(t, target, "files"), 1)
		}
		for _, route := range []string{"view", "download"} {
			req, e := http.NewRequestWithContext(ctx, "GET", f.Endpoint(harness.App).HTTP+"/file/"+route+"?invocation_id="+id+"&bytestream_url="+url.QueryEscape(uri), nil)
			require.NoError(t, e)
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			resp, e := http.DefaultClient.Do(req)
			require.NoError(t, e)
			b, e := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, e)
			require.Equal(t, 200, resp.StatusCode)
			require.Equal(t, content, b)
		}
	}
	_, e = grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "DeleteInvocation", map[string]any{"invocationId": id})
	require.NoError(t, e)
	for _, entry := range [][2]string{{"buildbuddy.service.BuildBuddyService", "GetTarget"}, {"api.v1.ApiService", "GetTarget"}, {"api.v1.ApiService", "GetAction"}} {
		req := map[string]any{"invocationId": id, "targetLabel": label}
		if entry[0] == "api.v1.ApiService" {
			req = map[string]any{"selector": map[string]any{"invocationId": id}}
		}
		_, e := grpcRead(t, ctx, c, entry[0], entry[1], req)
		require.Equal(t, codes.NotFound, status.Code(e))
	}
	// Deleting invocation metadata does not delete CAS bytes.
	for _, reader := range []*harness.Clients{c, proxy} {
		b, e := reader.Read(ctx, "", digest)
		require.NoError(t, e)
		require.Equal(t, content, b)
	}
}
