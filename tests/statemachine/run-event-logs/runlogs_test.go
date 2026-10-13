package runlogs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	cli := downloadCLI(t)
	for _, terminal := range []int64{1, 2, 4} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			id := upload(t, f, nil, 0)
			model := ""
			checkStatus := func(status int64) {
				b := unary(t, ctx, c, "GetInvocation", field(2, str(1, id)))
				invs := fields(b, 2)
				require.Len(t, invs, 1)
				require.Equal(t, uint64(status), value(invs[0], 39))
				w := web(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})
				inv := w["invocation"].([]any)[0].(map[string]any)
				names := map[int64]string{0: "UNKNOWN_OVERALL_STATUS", 1: "SUCCESS", 2: "FAILURE", 3: "IN_PROGRESS", 4: "DISCONNECTED"}
				if status == 0 {
					require.True(t, inv["runStatus"] == nil || inv["runStatus"] == names[status])
				} else {
					require.Equal(t, names[status], inv["runStatus"])
				}
			}
			checkLogs := func(complete bool) {
				require.Eventually(t, func() bool {
					cmd := exec.CommandContext(ctx, cli, "view", id, "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--lines=1000000")
					cmd.Dir = t.TempDir()
					// Only a newly provisioned fixture key reaches the CLI.
					cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + cmd.Dir, "BUILDBUDDY_API_KEY=" + f.APIKey}
					out, err := cmd.Output()
					return err == nil && string(out) == "===== RUN LOGS =====\n"+model
				}, 15*time.Second, 100*time.Millisecond, "fixture-local bb view must expose every accepted byte")
				var api []byte
				require.NoError(t, c.Conn.Invoke(ctx, "/api.v1.ApiService/GetLog", field(1, str(1, id)), &api, grpc.ForceCodec(wireCodec{})))
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
				if log, ok := v["log"].(map[string]any); ok {
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
				stream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, prefix+"GetEventLog", grpc.ForceCodec(wireCodec{}))
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
			for i := 0; i < 2; i++ {
				unary(t, ctx, c, "UpdateRunStatus", msg(str(2, id), num(3, 3)))
				checkStatus(3)
			}
			stream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, prefix+"WriteEventLog", grpc.ForceCodec(wireCodec{}))
			require.NoError(t, err)
			// Cross the 2 MB chunk boundary with valid newline-terminated UTF-8 logs;
			// repeated equal writes are append operations, not idempotent updates.
			for _, data := range []string{"first λ\n", strings.Repeat("run line\n", 250000), "repeat\n", "repeat\n", "last\n"} {
				require.NoError(t, stream.SendMsg(msg(num(2, 2), field(3, str(1, id)), field(4, []byte(data)))))
				model += data
			}
			require.NoError(t, stream.CloseSend())
			var response []byte
			require.NoError(t, stream.RecvMsg(&response))
			checkLogs(false)
			checkStatus(3)
			unary(t, ctx, c, "UpdateRunStatus", msg(str(2, id), num(3, terminal)))
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
