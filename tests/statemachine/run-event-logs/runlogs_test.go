package runlogs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func fields(b []byte, n protowire.Number) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		tag, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("bad response tag")
		}
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				panic("bad response bytes")
			}
			if tag == n {
				out = append(out, v)
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(tag, typ, b)
			if k < 0 {
				panic("bad field")
			}
			b = b[k:]
		}
	}
	return out
}
func value(b []byte, n protowire.Number) uint64 {
	for len(b) > 0 {
		tag, typ, k := protowire.ConsumeTag(b)
		b = b[k:]
		if tag == n && typ == protowire.VarintType {
			v, _ := protowire.ConsumeVarint(b)
			return v
		}
		k = protowire.ConsumeFieldValue(tag, typ, b)
		b = b[k:]
	}
	return 0
}

const prefix = "/buildbuddy.service.BuildBuddyService/"

func unary(t *testing.T, ctx context.Context, c *harness.Clients, method string, b []byte) []byte {
	t.Helper()
	var out []byte
	require.NoError(t, c.Conn.Invoke(ctx, prefix+method, b, &out, grpc.ForceCodec(wireCodec{})))
	return out
}
func web(t *testing.T, f *harness.Fixture, m string, r map[string]any) map[string]any {
	t.Helper()
	r["requestContext"] = f.Web.RequestContext(f.GroupID)
	out, err := f.Web.RPC(m, r)
	require.NoError(t, err)
	return out
}
func TestRunLogStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	second := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "second-run-log-client", []string{"CAS_WRITE", "CACHE_WRITE"}))
	cli := downloadCLI(t)
	checkBuildLogControl(t, f)
	for _, terminal := range []int64{1, 2, 4} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			parent := uploadMetadata(t, f, nil, 0, [][2]string{{"RUN_ID", "parent-" + fmt.Sprint(terminal)}})
			id := uploadMetadata(t, f, nil, 0, [][2]string{{"PARENT_RUN_ID", "parent-" + fmt.Sprint(terminal)}})
			model := ""
			rawJSON := artifactBytes(t, ctx, f, id, "raw_json")
			rawProto := artifactBytes(t, ctx, f, id, "raw_proto")
			checkStatus := func(status int64) {
				require.JSONEq(t, string(rawJSON), string(artifactBytes(t, ctx, f, id, "raw_json")), "BES JSON must remain unchanged by run mutations")
				require.Equal(t, canonicalRawProto(t, rawProto), canonicalRawProto(t, artifactBytes(t, ctx, f, id, "raw_proto")))
				checkSearch(t, ctx, f, second, id, status)
				for _, kind := range []string{"application/proto", "application/proto+prefixed"} {
					b := httpUnary(t, ctx, f, "GetInvocation", field(2, str(1, id)), kind)
					invs := fields(b, 2)
					require.Len(t, invs, 1)
					require.Equal(t, uint64(status), value(invs[0], 39))
				}
				pb := unary(t, ctx, second, "GetInvocation", field(2, msg(str(1, parent), num(2, 1))))
				children := fields(fields(pb, 2)[0], 36)
				require.Len(t, children, 1)
				require.Equal(t, uint64(status), value(children[0], 39))
				pw := web(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": parent, "fetchChildInvocations": true}})
				child := pw["invocation"].([]any)[0].(map[string]any)["childInvocations"].([]any)[0].(map[string]any)
				names := map[int64]string{1: "SUCCESS", 2: "FAILURE", 3: "IN_PROGRESS", 4: "DISCONNECTED"}
				if status != 0 {
					require.Equal(t, names[status], child["runStatus"])
				}
				b := unary(t, ctx, c, "GetInvocation", field(2, str(1, id)))
				invs := fields(b, 2)
				require.Len(t, invs, 1)
				require.Equal(t, uint64(status), value(invs[0], 39))
				w := web(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})
				inv := w["invocation"].([]any)[0].(map[string]any)
				names = map[int64]string{0: "UNKNOWN_OVERALL_STATUS", 1: "SUCCESS", 2: "FAILURE", 3: "IN_PROGRESS", 4: "DISCONNECTED"}
				if status == 0 {
					require.True(t, inv["runStatus"] == nil || inv["runStatus"] == names[status])
				} else {
					require.Equal(t, names[status], inv["runStatus"])
				}
			}
			checkLogs := func(complete bool) {
				started := time.Now()
				require.Eventually(t, func() bool {
					cmd := exec.CommandContext(ctx, cli, "view", id, "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--lines=1000000")
					cmd.Dir = t.TempDir()
					// Only a newly provisioned fixture key reaches the CLI.
					cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + cmd.Dir, "BUILDBUDDY_API_KEY=" + f.APIKey}
					out, err := cmd.Output()
					return err == nil && string(out) == "===== RUN LOGS =====\n"+model
				}, 15*time.Second, 100*time.Millisecond, "fixture-local bb view must expose every accepted byte")
				t.Logf("CLI convergence: %s", time.Since(started))
				checkDownload(t, ctx, f, id, model)
				checkHTTPStream(t, ctx, f, id, model, complete)
				var api []byte
				require.NoError(t, c.Conn.Invoke(ctx, "/api.v1.ApiService/GetLog", field(1, str(1, id)), &api, grpc.ForceCodec(wireCodec{})))
				require.Len(t, fields(api, 1), 1, "GetLog must return a log object")
				for _, log := range fields(api, 1) {
					require.Empty(t, fields(log, 3), "v1 GetLog exposes build logs only")
				}
				body, err := json.Marshal(map[string]any{"selector": map[string]any{"invocationId": id}})
				require.NoError(t, err)
				req, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/GetLog", bytes.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-buildbuddy-api-key", f.APIKey)
				res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
				require.NoError(t, err)
				raw, err := io.ReadAll(res.Body)
				res.Body.Close()
				require.NoError(t, err)
				require.Equal(t, 200, res.StatusCode, string(raw))
				var v map[string]any
				require.NoError(t, json.Unmarshal(raw, &v))
				log, ok := v["log"].(map[string]any)
				require.True(t, ok, "REST GetLog must return a log object")
				if ok {
					require.True(t, log["contents"] == nil || log["contents"] == "")
				}

				// Poll the first read until the writer's deferred close is durably visible.
				require.Eventually(t, func() bool {
					var out []byte
					err := c.Conn.Invoke(ctx, prefix+"GetEventLogChunk", msg(str(2, id), str(3, "0000"), num(5, 2)), &out, grpc.ForceCodec(wireCodec{}))
					return err == nil && len(fields(out, 4)) > 0
				}, 10*time.Second, 50*time.Millisecond)
				var got strings.Builder
				token := "0000"
				for i := 0; i < 20; i++ {
					b := unary(t, ctx, c, "GetEventLogChunk", msg(str(2, id), str(3, token), num(5, 2)))
					buffers := fields(b, 4)
					if len(buffers) > 0 {
						got.Write(buffers[0])
					}
					w := web(t, f, "GetEventLogChunk", map[string]any{"invocationId": id, "chunkId": token, "type": "RUN_LOG"})
					var wb []byte
					if s, ok := w["buffer"].(string); ok {
						var err error
						wb, err = base64.StdEncoding.DecodeString(s)
						require.NoError(t, err)
					}
					if len(buffers) > 0 {
						require.Equal(t, buffers[0], wb)
					} else {
						require.Empty(t, wb)
					}
					for n, name := range map[protowire.Number]string{2: "previousChunkId", 3: "nextChunkId"} {
						expected := ""
						if v := fields(b, n); len(v) > 0 {
							expected = string(v[0])
						}
						actual, _ := w[name].(string)
						require.Equal(t, expected, actual, name)
					}
					live, _ := w["live"].(bool)
					require.Equal(t, value(b, 5) != 0, live)
					next := fields(b, 3)
					if len(next) == 0 || string(next[0]) == token {
						break
					}
					token = string(next[0])
					require.Less(t, i, 19)
				}
				require.Equal(t, model, got.String())
				// Streaming live tails replace the same chunk rather than append it. Starting
				// after a closed writer gives an immutable prefix and a possibly empty tail.
				streamCtx, stopStream := context.WithCancel(ctx)
				defer stopStream()
				stream, err := c.Conn.NewStream(streamCtx, &grpc.StreamDesc{ServerStreams: true}, prefix+"GetEventLog", grpc.ForceCodec(wireCodec{}))
				require.NoError(t, err)
				require.NoError(t, stream.SendMsg(msg(str(2, id), str(3, "0000"), num(5, 2))))
				require.NoError(t, stream.CloseSend())
				var streamed strings.Builder
				for i := 0; i < 20; i++ {
					var b []byte
					err := stream.RecvMsg(&b)
					if err == io.EOF {
						break
					}
					require.NoError(t, err)
					if v := fields(b, 4); len(v) > 0 {
						streamed.Write(v[0])
					}
					if streamed.Len() >= len(model) {
						break
					}
				}
				require.Equal(t, model, streamed.String())
				if !complete {
					done := make(chan error, 1)
					go func() {
						for {
							var b []byte
							if err := stream.RecvMsg(&b); err != nil {
								done <- err
								return
							}
							for _, data := range fields(b, 4) {
								if len(data) > 0 {
									done <- fmt.Errorf("duplicate stream bytes")
									return
								}
							}
						}
					}()
					select {
					case err := <-done:
						t.Fatalf("IN_PROGRESS stream closed or repeated data: %v", err)
					case <-time.After(350 * time.Millisecond):
					}
					stopStream()
					<-done
				}
				if complete {
					ended := false
					for i := 0; i < 3; i++ {
						var tail []byte
						err := stream.RecvMsg(&tail)
						if err == io.EOF {
							ended = true
							break
						}
						require.NoError(t, err)
						for _, b := range fields(tail, 4) {
							require.Empty(t, b, "no duplicated bytes after complete model")
						}
					}
					require.True(t, ended, "terminal run stream must end after any empty final chunk")
				}
			}
			checkStatus(0)
			checkHTTPStream(t, ctx, f, id, "", true)
			for i := 0; i < 2; i++ {
				unary(t, ctx, c, "UpdateRunStatus", msg(str(2, id), num(3, 3)))
				checkStatus(3)
				checkHTTPStream(t, ctx, f, id, "", false)
			}
			stream, err := second.Conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, prefix+"WriteEventLog", grpc.ForceCodec(wireCodec{}))
			require.NoError(t, err)
			// Cross the 2 MB chunk boundary with valid newline-terminated UTF-8 logs;
			// repeated equal writes are append operations, not idempotent updates.
			for _, data := range []string{"first λ\n", strings.Repeat("run line\n", 250000), "repeat\n", "repeat\n", "last\n"} {
				require.NoError(t, stream.SendMsg(msg(num(2, 2), field(3, str(1, id)), field(4, []byte(data)))))
				model += data
			}
			// Observe the active writer's live tail before committing its close.
			liveStarted := time.Now()
			var liveToken string
			var liveResponse []byte
			require.Eventually(t, func() bool {
				token := "0000"
				var got strings.Builder
				for i := 0; i < 20; i++ {
					var b []byte
					err := c.Conn.Invoke(ctx, prefix+"GetEventLogChunk", msg(str(2, id), str(3, token), num(5, 2)), &b, grpc.ForceCodec(wireCodec{}))
					if err != nil {
						return false
					}
					for _, data := range fields(b, 4) {
						got.Write(data)
					}
					if value(b, 5) == 1 {
						liveToken = token
						liveResponse = b
						return got.String() == model
					}
					next := fields(b, 3)
					if len(next) == 0 || string(next[0]) == token {
						return false
					}
					token = string(next[0])
				}
				return false
			}, 15*time.Second, 50*time.Millisecond)
			t.Logf("active writer live-tail convergence: %s", time.Since(liveStarted))
			lw := web(t, f, "GetEventLogChunk", map[string]any{"invocationId": id, "chunkId": liveToken, "type": "RUN_LOG"})
			lb, err := base64.StdEncoding.DecodeString(lw["buffer"].(string))
			require.NoError(t, err)
			require.Equal(t, fields(liveResponse, 4)[0], lb)
			require.Equal(t, true, lw["live"])
			require.Equal(t, liveToken, lw["nextChunkId"])
			checkStatus(3)
			require.NoError(t, stream.CloseSend())
			var response []byte
			require.NoError(t, stream.RecvMsg(&response))
			checkLogs(false)
			// Each writer starts at chunk zero (NewEventLogWriter / chunkstore.NewWriter).
			// Replay identical complete content from the other same-org client.
			replay, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, prefix+"WriteEventLog", grpc.ForceCodec(wireCodec{}))
			require.NoError(t, err)
			require.NoError(t, replay.SendMsg(msg(num(2, 2), field(3, str(1, id)), field(4, []byte(model)))))
			require.NoError(t, replay.CloseSend())
			var replayResponse []byte
			require.NoError(t, replay.RecvMsg(&replayResponse))
			checkLogs(false)
			checkStatus(3)
			unary(t, ctx, second, "UpdateRunStatus", msg(str(2, id), num(3, 3)))
			checkStatus(3)
			checkLogs(false)
			// Keep a reader open across the status mutation, rather than reopening it.
			followCtx, stopFollow := context.WithTimeout(ctx, 10*time.Second)
			defer stopFollow()
			follow, err := c.Conn.NewStream(followCtx, &grpc.StreamDesc{ServerStreams: true}, prefix+"GetEventLog", grpc.ForceCodec(wireCodec{}))
			require.NoError(t, err)
			require.NoError(t, follow.SendMsg(msg(str(2, id), num(4, 1000000), num(5, 2))))
			require.NoError(t, follow.CloseSend())
			var initial []byte
			require.NoError(t, follow.RecvMsg(&initial))
			require.Equal(t, model, string(fields(initial, 4)[0]))
			finished := make(chan error, 1)
			go func() {
				for {
					var b []byte
					err := follow.RecvMsg(&b)
					if err != nil {
						finished <- err
						return
					}
					for _, data := range fields(b, 4) {
						if len(data) > 0 {
							finished <- fmt.Errorf("follow stream duplicated data")
							return
						}
					}
				}
			}()
			select {
			case err := <-finished:
				t.Fatalf("reader closed before terminal status: %v", err)
			case <-time.After(350 * time.Millisecond):
			}
			unary(t, ctx, c, "UpdateRunStatus", msg(str(2, id), num(3, terminal)))
			require.Equal(t, io.EOF, <-finished, "already-open reader must observe terminal status")
			checkStatus(terminal)
			checkLogs(true)
			time.Sleep(350 * time.Millisecond)
			checkStatus(terminal)
			checkLogs(true)
		})
	}
}

func downloadCLI(t *testing.T) string {
	t.Helper()
	client := &http.Client{Timeout: time.Minute}
	res, err := client.Get("https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	b, err := io.ReadAll(io.LimitReader(res.Body, 100<<20))
	require.NoError(t, err)
	require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(b)))
	path := filepath.Join(t.TempDir(), "bb")
	require.NoError(t, os.WriteFile(path, b, 0755))
	return path
}

func checkSearch(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, id string, status int64) {
	t.Helper()
	b := unary(t, ctx, c, "SearchInvocation", field(2, str(3, f.GroupID)))
	for _, kind := range []string{"application/proto", "application/proto+prefixed"} {
		response := httpUnary(t, ctx, f, "SearchInvocation", field(2, str(3, f.GroupID)), kind)
		found := false
		for _, inv := range fields(response, 2) {
			ids := fields(inv, 1)
			if len(ids) > 0 && string(ids[0]) == id {
				found = true
				require.Equal(t, uint64(status), value(inv, 39))
			}
		}
		require.True(t, found)
	}
	found := false
	for _, inv := range fields(b, 2) {
		if ids := fields(inv, 1); len(ids) > 0 && string(ids[0]) == id {
			found = true
			require.Equal(t, uint64(status), value(inv, 39))
		}
	}
	require.True(t, found)
	w := web(t, f, "SearchInvocation", map[string]any{"query": map[string]any{"groupId": f.GroupID}})
	found = false
	for _, v := range w["invocation"].([]any) {
		inv := v.(map[string]any)
		if inv["invocationId"] == id {
			found = true
			if status != 0 {
				require.Equal(t, map[int64]string{1: "SUCCESS", 2: "FAILURE", 3: "IN_PROGRESS", 4: "DISCONNECTED"}[status], inv["runStatus"])
			}
		}
	}
	require.True(t, found)
}
func checkDownload(t *testing.T, ctx context.Context, f *harness.Fixture, id, model string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		req, err := http.NewRequestWithContext(ctx, "GET", f.Endpoint(harness.App).HTTP+"/file/download?artifact=runlog&invocation_id="+id, nil)
		require.NoError(t, err)
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		require.NoError(t, err)
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode)
		require.Equal(t, model, string(b))
	}
}
func checkHTTPStream(t *testing.T, ctx context.Context, f *harness.Fixture, id, model string, complete bool) {
	t.Helper()
	// Exercise the exact UI default and compare its tail and metadata to gRPC.
	w := web(t, f, "GetEventLogChunk", map[string]any{"invocationId": id, "chunkId": "", "minLines": 100000, "type": "RUN_LOG"})
	var wb []byte
	if encoded, ok := w["buffer"].(string); ok {
		var err error
		wb, err = base64.StdEncoding.DecodeString(encoded)
		require.NoError(t, err)
	}
	require.True(t, strings.HasSuffix(model, string(wb)))
	requiredLines := strings.Count(model, "\n")
	if requiredLines > 100000 {
		requiredLines = 100000
	}
	require.GreaterOrEqual(t, bytes.Count(wb, []byte("\n")), requiredLines)
	c := f.Clients(harness.App)
	b := unary(t, ctx, c, "GetEventLogChunk", msg(str(2, id), num(4, 100000), num(5, 2)))
	var gb []byte
	if buffers := fields(b, 4); len(buffers) > 0 {
		gb = buffers[0]
	}
	require.Equal(t, gb, wb)
	for n, name := range map[protowire.Number]string{2: "previousChunkId", 3: "nextChunkId"} {
		expected := ""
		if values := fields(b, n); len(values) > 0 {
			expected = string(values[0])
		}
		actual, _ := w[name].(string)
		require.Equal(t, expected, actual)
	}
	live, _ := w["live"].(bool)
	require.Equal(t, value(b, 5) != 0, live)
	// The UI's empty chunk ID means last minLines, rather than first chunk.
	request := msg(str(2, id), num(4, 1000000), num(5, 2))
	for _, contentType := range []string{"application/proto", "application/proto+prefixed"} {
		payload := request
		if contentType == "application/proto+prefixed" {
			payload = frame(request)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/rpc/BuildBuddyService/GetEventLogChunk", bytes.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		require.NoError(t, err)
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode, string(b))
		if contentType == "application/proto+prefixed" {
			require.GreaterOrEqual(t, len(b), 5)
			n := int(binary.BigEndian.Uint32(b[1:5]))
			require.GreaterOrEqual(t, len(b), 5+n)
			b = b[5 : 5+n]
		}
		buffers := fields(b, 4)
		var actual []byte
		if len(buffers) > 0 {
			actual = buffers[0]
		}
		require.Equal(t, model, string(actual))
	}
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, "POST", f.Endpoint(harness.App).HTTP+"/rpc/BuildBuddyService/GetEventLog", bytes.NewReader(frame(msg(str(2, id), str(3, "0000"), num(5, 2)))))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/proto+prefixed")
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	var got strings.Builder
	read := func() ([]byte, bool, error) {
		h := make([]byte, 5)
		_, err := io.ReadFull(res.Body, h)
		if err != nil {
			return nil, false, err
		}
		n := binary.BigEndian.Uint32(h[1:])
		if n > 4<<20 {
			return nil, false, fmt.Errorf("oversize frame")
		}
		b := make([]byte, n)
		_, err = io.ReadFull(res.Body, b)
		return b, h[0]&128 != 0, err
	}
	for got.Len() < len(model) {
		b, trailer, err := read()
		require.NoError(t, err)
		require.False(t, trailer, "stream ended before expected bytes")
		for _, data := range fields(b, 4) {
			got.Write(data)
		}
	}
	require.Equal(t, model, got.String())
	done := make(chan error, 1)
	go func() {
		for {
			b, trailer, err := read()
			if err != nil {
				done <- err
				return
			}
			if trailer {
				if !strings.Contains(strings.ToLower(string(b)), "grpc-status: 0") {
					done <- fmt.Errorf("bad trailers: %s", b)
				} else {
					done <- io.EOF
				}
				return
			}
			for _, data := range fields(b, 4) {
				if len(data) > 0 {
					done <- fmt.Errorf("duplicate bytes")
					return
				}
			}
		}
	}()
	if complete {
		require.Equal(t, io.EOF, <-done)
	} else {
		select {
		case err := <-done:
			t.Fatalf("HTTP IN_PROGRESS stream ended: %v", err)
		case <-time.After(350 * time.Millisecond):
		}
		cancel()
		<-done
	}
}
func frame(b []byte) []byte {
	out := make([]byte, 5, len(b)+5)
	binary.BigEndian.PutUint32(out[1:], uint32(len(b)))
	return append(out, b...)
}

func checkBuildLogControl(t *testing.T, f *harness.Fixture) {
	t.Helper()
	id := upload(t, f, []string{"build-log control λ\n"}, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var b []byte
	require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/api.v1.ApiService/GetLog", field(1, str(1, id)), &b, grpc.ForceCodec(wireCodec{})))
	logs := fields(b, 1)
	require.Len(t, logs, 1)
	contents := fields(logs[0], 3)
	require.Len(t, contents, 1)
	require.Equal(t, "build-log control λ\n", string(contents[0]))
	tokens := fields(b, 2)
	require.Len(t, tokens, 1)
	token := string(tokens[0])
	var page []byte
	require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/api.v1.ApiService/GetLog", msg(field(1, str(1, id)), str(3, token)), &page, grpc.ForceCodec(wireCodec{})))
	require.Len(t, fields(page, 1), 1)
	require.Empty(t, fields(fields(page, 1)[0], 3))
	require.Empty(t, fields(page, 2))
	req, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/GetLog", strings.NewReader(fmt.Sprintf(`{"selector":{"invocationId":%q}}`, id)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	var w map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&w))
	require.Equal(t, "build-log control λ\n", w["log"].(map[string]any)["contents"])
	require.Equal(t, token, w["nextPageToken"])
}

func httpUnary(t *testing.T, ctx context.Context, f *harness.Fixture, method string, payload []byte, kind string) []byte {
	t.Helper()
	if kind == "application/proto+prefixed" {
		payload = frame(payload)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/rpc/BuildBuddyService/"+method, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", kind)
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode, string(b))
	if kind == "application/proto+prefixed" {
		require.GreaterOrEqual(t, len(b), 5)
		n := int(binary.BigEndian.Uint32(b[1:5]))
		require.GreaterOrEqual(t, len(b), 5+n)
		require.Equal(t, byte(0), b[0])
		b = b[5 : 5+n]
	}
	return b
}

func artifactBytes(t *testing.T, ctx context.Context, f *harness.Fixture, id, artifact string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", f.Endpoint(harness.App).HTTP+"/file/download?artifact="+artifact+"&invocation_id="+id, nil)
	require.NoError(t, err)
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	require.NotEmpty(t, b)
	return b
}

// Protobuf map entry order is unspecified; normalize only the seeded metadata
// map, while preserving event ordering and every other field byte-for-byte.
func canonicalRawProto(t *testing.T, b []byte) []byte {
	t.Helper()
	var result []byte
	for len(b) > 0 {
		size, k := protowire.ConsumeVarint(b)
		require.Greater(t, k, 0)
		b = b[k:]
		require.LessOrEqual(t, size, uint64(len(b)))
		event := b[:int(size)]
		b = b[int(size):]
		var normalized []byte
		for len(event) > 0 {
			n, kind, k := protowire.ConsumeTag(event)
			require.Greater(t, k, 0)
			vlen := protowire.ConsumeFieldValue(n, kind, event[k:])
			require.GreaterOrEqual(t, vlen, 0)
			if n == 26 && kind == protowire.BytesType {
				payload, _ := protowire.ConsumeBytes(event[k:])
				entries := fields(payload, 1)
				sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i], entries[j]) < 0 })
				var metadata []byte
				for _, entry := range entries {
					metadata = append(metadata, field(1, entry)...)
				}
				normalized = append(normalized, field(26, metadata)...)
			} else {
				normalized = append(normalized, event[:k+vlen]...)
			}
			event = event[k+vlen:]
		}
		result = protowire.AppendVarint(result, uint64(len(normalized)))
		result = append(result, normalized...)
	}
	return result
}
