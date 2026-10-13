package executionstate_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	lropb "google.golang.org/genproto/googleapis/longrunning"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func wireRows(t *testing.T, b []byte, want protowire.Number) [][]byte {
	t.Helper()
	var rows [][]byte
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		size := protowire.ConsumeFieldValue(n, typ, b)
		require.GreaterOrEqual(t, size, 0)
		if n == want {
			v, k := protowire.ConsumeBytes(b)
			require.Positive(t, k)
			rows = append(rows, v)
		}
		b = b[size:]
	}
	return rows
}

func httpProto(t *testing.T, ctx context.Context, f *harness.Fixture, method string, body []byte) []byte {
	t.Helper()
	streaming := method == "GetTree"
	if streaming {
		frame := make([]byte, 5)
		binary.BigEndian.PutUint32(frame[1:], uint32(len(body)))
		body = append(frame, body...)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/protobuf")
	if streaming {
		req.Header.Set("Content-Type", "application/proto+prefixed")
	}
	res, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode, string(data))
	if streaming {
		var messages [][]byte
		for len(data) > 0 {
			require.GreaterOrEqual(t, len(data), 5)
			size := int(binary.BigEndian.Uint32(data[1:5]))
			require.GreaterOrEqual(t, len(data), 5+size)
			if data[0] == 0 {
				messages = append(messages, data[5:5+size])
			} else {
				require.EqualValues(t, 0x80, data[0])
				require.Contains(t, string(data[5:5+size]), "Grpc-Status: 0")
			}
			data = data[5+size:]
		}
		require.Len(t, messages, 1)
		return messages[0]
	}
	return data
}

// Every terminal state is read three times over a stability interval. The
// executor-produced stream supplies timing fields; command semantics supply the
// independently asserted exit status, output digest/content and stdout.
func completeSurfaces(t *testing.T, ctx context.Context, f *harness.Fixture, instance, invocation, name string, want *repb.ExecuteResponse) {
	t.Helper()
	key := f.Web.CreateAPIKey(t, f.GroupID, "execution-state-reader", []string{"CACHE_WRITE", "REGISTER_EXECUTOR"})
	second := harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
	started := time.Now()
	grpcProjection(t, ctx, f.Clients(harness.App), invocation, name, want)
	t.Logf("GetExecution convergence=%s (measured from terminal observation)", time.Since(started))
	lookup := append(field(1, []byte(invocation)), field(3, []byte(name))...)
	request := append(field(2, lookup), number(3, 1)...)
	webRequest := append(field(1, field(2, []byte(f.GroupID))), request...)
	for observation := 0; observation < 3; observation++ {
		if observation > 0 {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
		res := &emptypb.Empty{}
		require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetExecution", envelope(request), res))
		rows := wireRows(t, res.ProtoReflect().GetUnknown(), 2)
		require.Len(t, rows, 1)
		httpRows := wireRows(t, httpProto(t, ctx, f, "GetExecution", webRequest), 2)
		require.Len(t, httpRows, 1)
		require.Equal(t, rows[0], httpRows[0], "all HTTP/gRPC execution fields must agree")
		other := &emptypb.Empty{}
		require.NoError(t, second.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetExecution", envelope(request), other))
		require.Equal(t, res.ProtoReflect().GetUnknown(), other.ProtoReflect().GetUnknown(), "second org API key")
		row := rows[0]
		got := &repb.ExecuteResponse{}
		require.NoError(t, proto.Unmarshal(bytesField(t, row, 13), got))
		require.True(t, responseEqual(want, got))
		require.EqualValues(t, want.GetResult().GetExitCode(), wireNumber(t, row, 8), "top-level exit code")
		require.NotEmpty(t, bytesField(t, row, 7), "command snippet")
		topStatus := bytesField(t, row, 3)
		inlineStatus, _ := proto.Marshal(got.GetStatus())
		require.True(t, bytes.Equal(inlineStatus, topStatus), "top-level status")
		md := &repb.ExecutedActionMetadata{}
		require.NoError(t, proto.Unmarshal(bytesField(t, row, 5), md))
		compareDBMetadata(t, md, got.GetResult().GetExecutionMetadata())
		responseDigest := &repb.Digest{}
		require.NoError(t, proto.Unmarshal(bytesField(t, row, 12), responseDigest))
		require.True(t, proto.Equal(harness.Digest([]byte(name)), responseDigest))
		downloadsReq := append(field(2, []byte(invocation)), field(3, []byte(name))...)
		downloads := &emptypb.Empty{}
		require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetExecutionDownloads", envelope(downloadsReq), downloads))
		webDownloads := httpProto(t, ctx, f, "GetExecutionDownloads", append(field(1, field(2, []byte(f.GroupID))), downloadsReq...))
		require.True(t, bytes.Equal(downloads.ProtoReflect().GetUnknown(), webDownloads))
		require.Empty(t, wireRows(t, webDownloads, 2), "default fixture has no input-fetch experiment")
		auditCachedResponse(t, ctx, f, instance, invocation, name, want)
		replayBB(t, ctx, f.Clients(harness.App), name, want)
	}
	executionCLI(t, ctx, f, name, want)
	httpWait(t, ctx, f, name, want)
	executionProfile(t, ctx, f, invocation, name, want.GetResult().GetExecutionMetadata())
	t.Logf("terminal projections stable for %s", time.Since(started))
}

func httpWait(t *testing.T, ctx context.Context, f *harness.Fixture, name string, want *repb.ExecuteResponse) {
	body := append(field(1, field(2, []byte(f.GroupID))), field(2, []byte(name))...)
	frame := make([]byte, 5)
	binary.BigEndian.PutUint32(frame[1:], uint32(len(body)))
	frame = append(frame, body...)
	req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/WaitExecution", bytes.NewReader(frame))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/proto+prefixed")
	res, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	for {
		header := make([]byte, 5)
		_, err := io.ReadFull(res.Body, header)
		require.NoError(t, err)
		require.Zero(t, header[0])
		size := binary.BigEndian.Uint32(header[1:])
		require.Less(t, size, uint32(16<<20))
		data := make([]byte, size)
		_, err = io.ReadFull(res.Body, data)
		require.NoError(t, err)
		op := &lropb.Operation{}
		require.NoError(t, proto.Unmarshal(bytesField(t, data, 2), op))
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

func wireNumber(t *testing.T, b []byte, want protowire.Number) uint64 {
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		size := protowire.ConsumeFieldValue(n, typ, b)
		require.GreaterOrEqual(t, size, 0)
		if n == want {
			v, k := protowire.ConsumeVarint(b)
			require.Positive(t, k)
			return v
		}
		b = b[size:]
	}
	return 0
}

func downloadSurface(t *testing.T, ctx context.Context, f *harness.Fixture, invocation, instance string, d *repb.Digest, want []byte) {
	t.Helper()
	for _, endpoint := range []string{"/file/view", "/file/download"} {
		params := url.Values{"bytestream_url": {fmt.Sprintf("bytestream://app:1985/%sblobs/%s/%d", instance, d.Hash, d.SizeBytes)}, "invocation_id": {invocation}, "request_context": {base64.StdEncoding.EncodeToString(field(2, []byte(f.GroupID)))}}
		req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+endpoint+"?"+params.Encode(), nil)
		require.NoError(t, err)
		res, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode, string(data))
		require.Equal(t, want, data)
	}
}

func uiInputTree(t *testing.T, ctx context.Context, f *harness.Fixture, root, input *repb.Digest) {
	t.Helper()
	rootBytes, err := proto.Marshal(root)
	require.NoError(t, err)
	for _, method := range []string{"GetTree", "GetTreeDirectorySizes"} {
		fieldNumber := protowire.Number(3)
		if method == "GetTreeDirectorySizes" {
			fieldNumber = 2
		}
		req := field(fieldNumber, rootBytes)
		res := &emptypb.Empty{}
		require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, envelope(req), res))
		webReq := append(field(1, field(2, []byte(f.GroupID))), req...)
		data := httpProto(t, ctx, f, method, webReq)
		require.True(t, bytes.Equal(res.ProtoReflect().GetUnknown(), data))
		rows := wireRows(t, data, 2)
		require.Len(t, rows, 1)
		if method == "GetTree" {
			dir := &repb.Directory{}
			require.NoError(t, proto.Unmarshal(rows[0], dir))
			require.True(t, proto.Equal(&repb.Directory{Files: []*repb.FileNode{{Name: "input.txt", Digest: input}}}, dir))
		} else {
			require.EqualValues(t, 1, wireNumber(t, rows[0], 3))
			require.EqualValues(t, root.SizeBytes+input.SizeBytes, wireNumber(t, rows[0], 2))
		}
	}
}

// SQL projections intentionally store microseconds and only selected metadata.
// Compare each exposed standard timestamp at that documented precision. SQL
// represents unset timestamps as Unix epoch, not absent message fields.
func compareDBMetadata(t *testing.T, got, want *repb.ExecutedActionMetadata) {
	require.Equal(t, want.GetWorker(), got.GetWorker())
	pairs := [][2]*timestamppb.Timestamp{
		{got.GetQueuedTimestamp(), want.GetQueuedTimestamp()},
		{got.GetWorkerStartTimestamp(), want.GetWorkerStartTimestamp()},
		{got.GetWorkerCompletedTimestamp(), want.GetWorkerCompletedTimestamp()},
		{got.GetInputFetchStartTimestamp(), want.GetInputFetchStartTimestamp()},
		{got.GetInputFetchCompletedTimestamp(), want.GetInputFetchCompletedTimestamp()},
		{got.GetExecutionStartTimestamp(), want.GetExecutionStartTimestamp()},
		{got.GetExecutionCompletedTimestamp(), want.GetExecutionCompletedTimestamp()},
		{got.GetOutputUploadStartTimestamp(), want.GetOutputUploadStartTimestamp()},
		{got.GetOutputUploadCompletedTimestamp(), want.GetOutputUploadCompletedTimestamp()},
	}
	for i, p := range pairs {
		var expected int64
		if p[1] != nil {
			expected = p[1].AsTime().UnixMicro()
		}
		require.Equal(t, expected, p[0].AsTime().UnixMicro(), "SQL timestamp %d", i)
	}
}

func executionList(t *testing.T, ctx context.Context, f *harness.Fixture, invocation, hash string, names []string) {
	for _, filter := range []string{hash, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"} {
		lookup := append(field(1, []byte(invocation)), field(2, []byte(filter))...)
		request := field(2, lookup)
		grpcResult := &emptypb.Empty{}
		require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetExecution", envelope(request), grpcResult))
		httpResult := httpProto(t, ctx, f, "GetExecution", append(field(1, field(2, []byte(f.GroupID))), request...))
		grpcRows := wireRows(t, grpcResult.ProtoReflect().GetUnknown(), 2)
		httpRows := wireRows(t, httpResult, 2)
		if filter != hash {
			require.Empty(t, grpcRows)
			require.Empty(t, httpRows)
			continue
		}
		require.Len(t, grpcRows, len(names))
		require.Len(t, httpRows, len(names))
		var grpcNames, httpNames []string
		for _, row := range grpcRows {
			grpcNames = append(grpcNames, string(bytesField(t, row, 14)))
		}
		for _, row := range httpRows {
			httpNames = append(httpNames, string(bytesField(t, row, 14)))
		}
		require.ElementsMatch(t, names, grpcNames)
		require.ElementsMatch(t, names, httpNames)
	}
}

func executionProfile(t *testing.T, ctx context.Context, f *harness.Fixture, invocation, name string, md *repb.ExecutedActionMetadata) {
	params := url.Values{"artifact": {"execution_profile"}, "invocation_id": {invocation}, "execution_id": {name}, "request_context": {base64.StdEncoding.EncodeToString(field(2, []byte(f.GroupID)))}}
	req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?"+params.Encode(), nil)
	require.NoError(t, err)
	res, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode, string(data))
	var profile struct {
		Events []struct {
			Name      string `json:"name"`
			Timestamp int64  `json:"ts"`
			Duration  int64  `json:"dur"`
		} `json:"traceEvents"`
	}
	require.NoError(t, json.Unmarshal(data, &profile))
	require.GreaterOrEqual(t, len(profile.Events), 2)
	var commandEvents int
	for _, event := range profile.Events {
		if event.Name != "run command" {
			continue
		}
		commandEvents++
		start := min(md.GetQueuedTimestamp().AsTime().UnixMicro(), md.GetWorkerStartTimestamp().AsTime().UnixMicro())
		require.Equal(t, md.GetExecutionStartTimestamp().AsTime().UnixMicro()-start, event.Timestamp)
		require.Equal(t, max(int64(0), md.GetExecutionCompletedTimestamp().AsTime().Sub(md.GetExecutionStartTimestamp().AsTime()).Microseconds()), event.Duration)
	}
	expected := 0
	if md.GetExecutionStartTimestamp() != nil && md.GetExecutionCompletedTimestamp() != nil {
		expected = 1
	}
	require.Equal(t, expected, commandEvents)
}
