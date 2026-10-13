package schedulerstate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Release-specific wire envelopes avoid depending on BuildBuddy generated Go
// code. Field numbers are from v2.314.0 proto/scheduler.proto and
// proto/remote_execution.proto. Empty carries only known-to-server wire fields.
func msg(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func str(n protowire.Number, s string) []byte { return msg(n, []byte(s)) }
func num(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func envelope(b []byte) *emptypb.Empty {
	m := &emptypb.Empty{}
	m.ProtoReflect().SetUnknown(b)
	return m
}
func fields(t *testing.T, b []byte, want protowire.Number) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			require.Positive(t, k)
			if n == want {
				out = append(out, v)
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			require.Positive(t, k)
			b = b[k:]
		}
	}
	return out
}
func scalar(t *testing.T, b []byte, want protowire.Number) uint64 {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if n == want {
			require.Equal(t, protowire.VarintType, typ)
			v, k := protowire.ConsumeVarint(b)
			require.Positive(t, k)
			return v
		}
		k = protowire.ConsumeFieldValue(n, typ, b)
		require.Positive(t, k)
		b = b[k:]
	}
	return 0
}
func one(t *testing.T, b []byte, n protowire.Number) []byte {
	t.Helper()
	v := fields(t, b, n)
	if len(v) == 0 {
		return nil
	}
	require.Len(t, v, 1)
	return v[0]
}
func invoke(t *testing.T, ctx context.Context, c *harness.Clients, method string, b []byte) []byte {
	t.Helper()
	out := &emptypb.Empty{}
	require.NoError(t, c.Conn.Invoke(ctx, method, envelope(b), out))
	return out.ProtoReflect().GetUnknown()
}
func stream(t *testing.T, ctx context.Context, c *harness.Clients, method string) grpc.ClientStream {
	t.Helper()
	s, e := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/scheduler.Scheduler/"+method)
	require.NoError(t, e)
	return s
}
func send(t *testing.T, s grpc.ClientStream, b []byte) {
	t.Helper()
	require.NoError(t, s.SendMsg(envelope(b)))
}
func recv(t *testing.T, s grpc.ClientStream) []byte {
	t.Helper()
	m := &emptypb.Empty{}
	require.NoError(t, s.RecvMsg(m))
	return m.ProtoReflect().GetUnknown()
}

type model struct {
	exists        bool
	node          bool
	queue, active uint64
}

func (m model) checkTask(t *testing.T, ctx context.Context, c *harness.Clients, id string) {
	t.Helper()
	b := invoke(t, ctx, c, "/scheduler.Scheduler/TaskExists", str(1, id))
	require.Equal(t, m.exists, scalar(t, b, 1) != 0, "TaskExists %s", id)
}
func (m model) checkNode(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		b := invoke(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetExecutionNodes", msg(1, str(2, f.GroupID)))
		count := 0
		for _, entry := range fields(t, b, 2) {
			n := one(t, entry, 1)
			if string(one(t, n, 9)) == id {
				count++
				require.Equal(t, "model.local", string(one(t, n, 1)))
				require.Equal(t, "linux", string(one(t, n, 5)))
				require.Equal(t, "amd64", string(one(t, n, 6)))
				require.Equal(t, "scheduler-model", string(one(t, n, 7)))
				require.Equal(t, "v2.314.0", string(one(t, n, 8)))
				require.Equal(t, uint64(1<<30), scalar(t, n, 3))
				require.Equal(t, uint64(1000), scalar(t, n, 4))
				require.Equal(t, "none", string(one(t, n, 12)))
				require.NotEmpty(t, one(t, entry, 3), "last check-in timestamp")
				if scalar(t, n, 13) != m.queue || scalar(t, n, 14) != m.active {
					return false
				}
			}
		}
		web, e := f.Web.RPC("GetExecutionNodes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		require.NoError(t, e)
		wc := 0
		for _, v := range webList(web["executor"]) {
			n := v.(map[string]any)["node"].(map[string]any)
			if n["executorId"] == id {
				wc++
				require.Equal(t, "model.local", n["host"])
				require.Equal(t, "linux", n["osFamily"])
				require.Equal(t, "amd64", n["arch"])
				require.Equal(t, "scheduler-model", n["pool"])
				require.Equal(t, "v2.314.0", n["version"])
				require.Equal(t, "1073741824", n["assignableMemoryBytes"])
				require.Equal(t, "1000", n["assignableMilliCpu"])
				if jsonNumber(n["currentQueueLength"]) != m.queue || jsonNumber(n["activeActionCount"]) != m.active {
					return false
				}
			}
		}
		expected := 0
		if m.node {
			expected = 1
		}
		return count == expected && wc == expected
	}, 30*time.Second, 100*time.Millisecond, "registration model %+v", m)
}
func webList(v any) []any     { a, _ := v.([]any); return a }
func jsonNumber(v any) uint64 { f, _ := v.(float64); return uint64(f) }

func TestSchedulerStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	adminKey := f.Web.CreateAPIKey(t, f.GroupID, "scheduler-state-admin", []string{"CACHE_WRITE", "REGISTER_EXECUTOR", "ORG_ADMIN"})
	c := harness.Dial(t, f.Endpoint(harness.App).GRPC, adminKey)
	id := "scheduler-model-executor"
	pool := "scheduler-model"
	taskID := "scheduler-model-task"
	m := model{}
	m.checkNode(t, ctx, f, c, id)
	m.checkTask(t, ctx, c, taskID)
	reg := stream(t, ctx, c, "RegisterAndStreamWork")
	var regMu sync.Mutex
	register := func() {
		regMu.Lock()
		defer regMu.Unlock()
		send(t, reg, msg(1, msg(1, join(str(1, "model.local"), num(3, 1<<30), num(4, 1000), str(5, "linux"), str(6, "amd64"), str(7, pool), str(8, "v2.314.0"), str(9, id), msg(12, []byte("none")), num(13, m.queue), num(14, m.active), num(18, 1)))))
	}
	register()
	m.node = true
	m.checkNode(t, ctx, f, c, id)
	register()
	m.checkNode(t, ctx, f, c, id) // repeated registration must not duplicate
	// A receiver continuously acknowledges reservations, as a released executor
	// does. Only this goroutine writes ACKs while the main test awaits delivery.
	reservations := make(chan []byte, 32)
	cancellations := make(chan []byte, 8)
	moreWork := make(chan []byte, 8)
	receiveErrors := make(chan error, 1)
	go func() {
		for {
			out := &emptypb.Empty{}
			if e := reg.RecvMsg(out); e != nil {
				receiveErrors <- e
				return
			}
			b := out.ProtoReflect().GetUnknown()
			for _, v := range fields(t, b, 5) {
				cancellations <- v
			}
			for _, v := range fields(t, b, 4) {
				moreWork <- v
			}
			rs := fields(t, b, 3)
			for _, r := range rs {
				tid := string(one(t, r, 1))
				regMu.Lock()
				e := reg.SendMsg(envelope(msg(2, str(1, tid))))
				regMu.Unlock()
				if e != nil {
					receiveErrors <- e
					return
				}
				reservations <- r
			}
		}
	}()
	delivered := func() {
		t.Helper()
		select {
		case r := <-reservations:
			require.Equal(t, taskID, string(one(t, r, 1)))
			meta := one(t, r, 3)
			require.Equal(t, pool, string(one(t, meta, 4)))
		case e := <-receiveErrors:
			t.Fatalf("registration stream: %v", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	regMu.Lock()
	send(t, reg, msg(4, nil))
	regMu.Unlock()
	select {
	case r := <-moreWork:
		require.Positive(t, scalar(t, one(t, r, 1), 1))
	case e := <-receiveErrors:
		t.Fatal(e)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	m.checkNode(t, ctx, f, c, id)
	m.checkTask(t, ctx, c, taskID)
	size := join(num(1, 1<<20), num(2, 10))
	metadata := join(msg(1, size), msg(13, size), str(2, "linux"), str(3, "amd64"), str(4, pool), str(6, f.GroupID))
	command := &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf model"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}, {Name: "retry", Value: "true"}}}}
	root, e := c.UploadProto(ctx, "scheduler-model", &repb.Directory{})
	require.NoError(t, e)
	cd, e := c.UploadProto(ctx, "scheduler-model", command)
	require.NoError(t, e)
	action := &repb.Action{CommandDigest: cd, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second)}
	ad, e := c.UploadProto(ctx, "scheduler-model", action)
	require.NoError(t, e)
	marshal := func(p proto.Message) []byte { b, e := proto.Marshal(p); require.NoError(t, e); return b }
	payload := join(msg(1, marshal(&repb.ExecuteRequest{InstanceName: "scheduler-model", ActionDigest: ad, DigestFunction: repb.DigestFunction_SHA256})), msg(4, marshal(action)), msg(5, marshal(command)), str(6, taskID))
	for cycle := 0; cycle < 2; cycle++ {
		t.Logf("scheduler lifecycle cycle %d", cycle)
		invoke(t, ctx, c, "/scheduler.Scheduler/ScheduleTask", join(str(1, taskID), msg(2, metadata), msg(3, payload)))
		m.exists = true
		duplicate := &emptypb.Empty{}
		err := c.Conn.Invoke(ctx, "/scheduler.Scheduler/ScheduleTask", envelope(join(str(1, taskID), msg(2, metadata), msg(3, payload))), duplicate)
		require.Equal(t, codes.AlreadyExists, status.Code(err))
		m.checkTask(t, ctx, c, taskID)
		delivered()
		m.queue = 1
		register()
		m.checkTask(t, ctx, c, taskID)
		m.checkNode(t, ctx, f, c, id)
		reservation := join(str(1, taskID), msg(2, size), msg(3, metadata), str(4, id))
		for i := 0; i < 2; i++ {
			invoke(t, ctx, c, "/scheduler.Scheduler/EnqueueTaskReservation", reservation)
			delivered()
			m.checkTask(t, ctx, c, taskID)
		}
		leaseRequest := join(str(1, taskID), str(4, id), str(9, "model.local"))
		lease := stream(t, ctx, c, "LeaseTask")
		send(t, lease, leaseRequest)
		response := recv(t, lease)
		checkPayload(t, payload, one(t, response, 1))
		m.queue = 0
		m.active = 1
		register()
		m.checkNode(t, ctx, f, c, id)
		require.Positive(t, scalar(t, response, 2))
		leaseID := string(one(t, response, 5))
		require.NotEmpty(t, leaseID)
		m.checkTask(t, ctx, c, taskID)
		// Repeated valid renewal must keep the task alive and omit initial payload.
		for i := 0; i < 2; i++ {
			send(t, lease, leaseRequest)
			r := recv(t, lease)
			require.Empty(t, one(t, r, 1))
			require.Positive(t, scalar(t, r, 2))
			m.checkTask(t, ctx, c, taskID)
		}
		// Release the lease cleanly before the unary handoff. The scheduler
		// retains the last lease ID; ReEnqueueTask supports already-unclaimed
		// tasks (the backwards-compatible release protocol).
		send(t, lease, join(leaseRequest, num(3, 1)))
		require.Equal(t, uint64(1), scalar(t, recv(t, lease), 3))
		require.NoError(t, lease.CloseSend())
		m.checkTask(t, ctx, c, taskID)
		invoke(t, ctx, c, "/scheduler.Scheduler/ReEnqueueTask", join(str(1, taskID), str(2, "normal executor handoff"), str(3, leaseID)))
		delivered()
		m.active = 0
		m.queue = 1
		register()
		m.checkNode(t, ctx, f, c, id)
		m.checkTask(t, ctx, c, taskID)
		lease = stream(t, ctx, c, "LeaseTask")
		send(t, lease, leaseRequest)
		response = recv(t, lease)
		checkPayload(t, payload, one(t, response, 1))
		require.NotEqual(t, leaseID, string(one(t, response, 5)))
		m.active = 1
		m.queue = 0
		register()
		m.checkNode(t, ctx, f, c, id)
		m.checkTask(t, ctx, c, taskID)
		send(t, lease, join(leaseRequest, num(5, 1)))
		require.Equal(t, uint64(1), scalar(t, recv(t, lease), 3))
		delivered()
		m.active = 0
		m.queue = 1
		register()
		m.checkNode(t, ctx, f, c, id)
		m.checkTask(t, ctx, c, taskID)
		require.NoError(t, lease.CloseSend())
		lease = stream(t, ctx, c, "LeaseTask")
		send(t, lease, leaseRequest)
		checkPayload(t, payload, one(t, recv(t, lease), 1))
		m.active = 1
		m.queue = 0
		register()
		m.checkNode(t, ctx, f, c, id)
		m.checkTask(t, ctx, c, taskID)
		send(t, lease, join(leaseRequest, num(2, 1)))
		require.Equal(t, uint64(1), scalar(t, recv(t, lease), 3))
		require.NoError(t, lease.CloseSend())
		m.exists = false
		select {
		case r := <-cancellations:
			require.Equal(t, taskID, string(one(t, r, 1)))
		case e := <-receiveErrors:
			t.Fatal(e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		m.active = 0
		m.queue = 0
		register()
		m.checkNode(t, ctx, f, c, id)
		m.checkTask(t, ctx, c, taskID)
		m.checkTask(t, ctx, c, taskID)
	}
	// No ACK sender is active now; serialize all writes using an idle stream
	// before changing registration state.
	regMu.Lock()
	send(t, reg, msg(3, nil))
	regMu.Unlock()
	require.NoError(t, reg.CloseSend())
	m.node = false
	m.checkNode(t, ctx, f, c, id)
	testExecution(t, ctx, f, c)
}

func testExecution(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients) {
	t.Helper()
	root, e := c.UploadProto(ctx, "scheduler-real", &repb.Directory{})
	require.NoError(t, e)
	cmd, e := c.UploadProto(ctx, "scheduler-real", &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf lifecycle"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
	require.NoError(t, e)
	ad, e := c.UploadProto(ctx, "scheduler-real", &repb.Action{CommandDigest: cmd, InputRootDigest: root, DoNotCache: true, Timeout: durationpb.New(30 * time.Second)})
	require.NoError(t, e)
	invocation := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	rm, e := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: invocation})
	require.NoError(t, e)
	ctx = metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(rm))
	for i := 0; i < 2; i++ {
		s, e := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "scheduler-real", ActionDigest: ad, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, e)
		name := ""
		var result *repb.ExecuteResponse
		for {
			op, e := s.Recv()
			require.NoError(t, e)
			name = op.Name
			if op.Done {
				require.Nil(t, op.GetError())
				result = &repb.ExecuteResponse{}
				require.NoError(t, op.GetResponse().UnmarshalTo(result))
				break
			}
		}
		require.Zero(t, result.GetStatus().GetCode())
		require.Zero(t, result.GetResult().GetExitCode())
		stdout := result.GetResult().GetStdoutRaw()
		if d := result.GetResult().GetStdoutDigest(); d != nil {
			stdout, e = c.Read(ctx, "scheduler-real", d)
			require.NoError(t, e)
			proxy := f.Clients(harness.CacheProxy)
			b, e := proxy.Read(ctx, "scheduler-real", d)
			require.NoError(t, e)
			require.Equal(t, stdout, b)
		}
		require.Equal(t, "lifecycle", string(stdout))
		for j := 0; j < 2; j++ {
			wait, e := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
			require.NoError(t, e)
			for {
				op, e := wait.Recv()
				if e == io.EOF {
					t.Fatal("WaitExecution ended before completion")
				}
				require.NoError(t, e)
				if op.Done {
					other := &repb.ExecuteResponse{}
					require.NoError(t, op.GetResponse().UnmarshalTo(other))
					require.True(t, proto.Equal(result, other), fmt.Sprintf("Execute/WaitExecution %s", name))
					break
				}
			}
		}
		checkExecutionViews(t, ctx, f, c, invocation, name, result)
		require.Eventually(t, func() bool {
			b := invoke(t, ctx, c, "/scheduler.Scheduler/TaskExists", str(1, name))
			return scalar(t, b, 1) == 0
		}, 30*time.Second, 100*time.Millisecond, "finalized real execution task %s", name)
	}
}

// Lease-time experiment fields may legitimately be appended by the app.
func checkPayload(t *testing.T, want, got []byte) {
	t.Helper()
	for _, n := range []protowire.Number{1, 4, 5, 6} {
		require.Equal(t, one(t, want, n), one(t, got, n), "ExecutionTask field %d", n)
	}
}

func checkExecutionViews(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, invocation, id string, result *repb.ExecuteResponse) {
	t.Helper()
	require.Eventually(t, func() bool {
		b := invoke(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetExecution", join(msg(1, str(2, f.GroupID)), msg(2, join(str(1, invocation), str(3, id))), num(3, 1)))
		rows := fields(t, b, 2)
		if len(rows) != 1 {
			return false
		}
		r := rows[0]
		if string(one(t, r, 14)) != id || scalar(t, r, 2) != uint64(repb.ExecutionStage_COMPLETED) {
			return false
		}
		inline := &repb.ExecuteResponse{}
		if len(one(t, r, 13)) == 0 {
			return false
		}
		require.NoError(t, proto.Unmarshal(one(t, r, 13), inline))
		require.Equal(t, result.GetStatus().GetCode(), inline.GetStatus().GetCode())
		require.Equal(t, result.GetResult().GetExitCode(), inline.GetResult().GetExitCode())
		require.True(t, proto.Equal(result.GetResult().GetStdoutDigest(), inline.GetResult().GetStdoutDigest()), "stdout digest differs")
		require.Equal(t, result.GetResult().GetStdoutRaw(), inline.GetResult().GetStdoutRaw())
		digest := &repb.Digest{}
		require.NoError(t, proto.Unmarshal(one(t, r, 12), digest))
		require.True(t, proto.Equal(harness.Digest([]byte(id)), digest), "execution response digest")
		acReq := &repb.GetActionResultRequest{InstanceName: "scheduler-real", ActionDigest: digest, DigestFunction: repb.DigestFunction_SHA256}
		appAC, e := c.AC.GetActionResult(ctx, acReq)
		if status.Code(e) == codes.NotFound {
			return false
		}
		require.NoError(t, e)
		proxyAC, e := f.Clients(harness.CacheProxy).AC.GetActionResult(ctx, acReq)
		if status.Code(e) == codes.NotFound {
			return false
		}
		require.NoError(t, e)
		require.True(t, proto.Equal(appAC, proxyAC), "app/proxy execution response record")
		cached := &repb.ExecuteResponse{}
		require.NoError(t, proto.Unmarshal(appAC.GetStdoutRaw(), cached))
		require.Equal(t, result.GetStatus().GetCode(), cached.GetStatus().GetCode())
		require.Equal(t, result.GetResult().GetExitCode(), cached.GetResult().GetExitCode())
		require.True(t, proto.Equal(result.GetResult().GetStdoutDigest(), cached.GetResult().GetStdoutDigest()), "stdout digest differs")
		require.Equal(t, result.GetResult().GetStdoutRaw(), cached.GetResult().GetStdoutRaw())
		web, e := f.Web.RPC("GetExecution", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "executionLookup": map[string]any{"invocationId": invocation, "executionId": id}, "inlineExecuteResponse": true})
		require.NoError(t, e)
		a := webList(web["execution"])
		if len(a) != 1 {
			return false
		}
		row := a[0].(map[string]any)
		if row["executionId"] != id || row["stage"] != "COMPLETED" || row["executeResponse"] == nil {
			return false
		}
		// UI Any metadata includes release-specific messages; compare the standard
		// execution outcome fields after stripping auxiliary metadata recursively.
		stripAux(row["executeResponse"])
		raw, e := json.Marshal(row["executeResponse"])
		require.NoError(t, e)
		ui := &repb.ExecuteResponse{}
		require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, ui))
		require.Equal(t, result.GetStatus().GetCode(), ui.GetStatus().GetCode())
		require.Equal(t, result.GetResult().GetExitCode(), ui.GetResult().GetExitCode())
		require.True(t, proto.Equal(result.GetResult().GetStdoutDigest(), ui.GetResult().GetStdoutDigest()), "stdout digest differs")
		require.Equal(t, result.GetResult().GetStdoutRaw(), ui.GetResult().GetStdoutRaw())
		require.Equal(t, result.GetResult().GetExecutionMetadata().GetWorker(), ui.GetResult().GetExecutionMetadata().GetWorker())
		return true
	}, 30*time.Second, 100*time.Millisecond, "GetExecution gRPC/HTTP completion %s", id)
}
func stripAux(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "auxiliaryMetadata")
		for _, v := range x {
			stripAux(v)
		}
	case []any:
		for _, v := range x {
			stripAux(v)
		}
	}
}
