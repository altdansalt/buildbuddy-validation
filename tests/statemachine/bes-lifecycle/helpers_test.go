package bes_lifecycle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/grpcreflect"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

// Field numbers follow proto/build_event_stream.proto at v2.314.0. Encoding
// these few messages directly avoids adding the entire BuildBuddy module.
func msg(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func str(n protowire.Number, s string) []byte { return field(n, []byte(s)) }
func num(n protowire.Number, v int64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), uint64(v))
}
func event(id, payload protowire.Number, identity, data []byte) []byte {
	return msg(field(1, field(id, identity)), field(payload, data))
}

func rpc(t *testing.T, f *harness.Fixture, method string, req map[string]any) map[string]any {
	t.Helper()
	req["requestContext"] = f.Web.RequestContext(f.GroupID)
	res, err := f.Web.RPC(method, req)
	require.NoError(t, err)
	return res
}
func api(t *testing.T, f *harness.Fixture, method string, req map[string]any) (int, map[string]any) {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	r, err := http.NewRequest("POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(b))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-buildbuddy-api-key", f.APIKey)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(r)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, map[string]any{"error": string(body)}
	}
	var result map[string]any
	require.NoError(t, json.Unmarshal(body, &result), "HTTP %d: %s", resp.StatusCode, body)
	return resp.StatusCode, result
}
func objects(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	if m[key] == nil {
		return nil
	}
	a, ok := m[key].([]any)
	require.True(t, ok, "%v", m)
	return a
}

// Reflection obtains the released server's own schemas, avoiding a source build.
func grpcRead(t *testing.T, ctx context.Context, c *harness.Clients, service, method string, req map[string]any) (map[string]any, error) {
	t.Helper()
	rc := grpcreflect.NewClientAuto(ctx, c.Conn)
	defer rc.Reset()
	sd, err := rc.ResolveService(service)
	if err != nil {
		return nil, err
	}
	md := sd.FindMethodByName(method)
	if md == nil {
		return nil, fmt.Errorf("missing method %s", method)
	}
	in, out := dynamic.NewMessage(md.GetInputType()), dynamic.NewMessage(md.GetOutputType())
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err = in.UnmarshalJSON(b); err != nil {
		return nil, err
	}
	if err = c.Conn.Invoke(ctx, "/"+service+"/"+method, in, out, grpc.WaitForReady(false)); err != nil {
		return nil, err
	}
	b, err = out.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(b, &result)
	return result, err
}

func auditDownload(t *testing.T, ctx context.Context, f *harness.Fixture, id, artifact string) (int, []byte) {
	return artifactFetch(t, ctx, f, id, artifact, "download")
}

func artifactFetch(t *testing.T, ctx context.Context, f *harness.Fixture, id, artifact, route string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", f.Endpoint(harness.App).HTTP+"/file/"+route+"?invocation_id="+id+"&artifact="+artifact+"&attempt=1", nil)
	require.NoError(t, err)
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}
