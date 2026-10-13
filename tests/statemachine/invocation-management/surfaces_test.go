package invocation_management_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

func varintField(t *testing.T, b []byte, n protowire.Number) uint64 {
	t.Helper()
	for len(b) > 0 {
		k, typ, l := protowire.ConsumeTag(b)
		require.Positive(t, l)
		b = b[l:]
		z := protowire.ConsumeFieldValue(k, typ, b)
		require.Positive(t, z)
		if k == n && typ == protowire.VarintType {
			v, _ := protowire.ConsumeVarint(b)
			return v
		}
		b = b[z:]
	}
	return 0
}

// The target assertions are the audit probe, integrated at every model step.
func checkAdditionalSurfaces(t *testing.T, f *harness.Fixture, app, second, anon *harness.Clients, id string, exists, public bool) {
	t.Helper()
	req := msg(str(1, id), str(2, "//sample:test"))
	b, e := call(app, ui+"GetTarget", req)
	j, he := f.Web.RPC("GetTarget", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id, "targetLabel": "//sample:test"})
	ab, ae := call(anon, ui+"GetTarget", req)
	if exists {
		require.NoError(t, e)
		require.NoError(t, he)
		found := false
		for _, g := range bytesFields(t, b, 1) {
			for _, target := range bytesFields(t, g, 2) {
				if string(first(t, first(t, target, 1), 2)) == "//sample:test" {
					found = true
				}
			}
		}
		require.True(t, found)
		require.NotEmpty(t, objects(t, j, "targetGroups"))
		if public {
			require.NoError(t, ae)
			require.Equal(t, b, ab)
		} else {
			require.Equal(t, codes.Unauthenticated, status.Code(ae))
		}
	} else {
		require.Equal(t, codes.NotFound, status.Code(e))
		require.ErrorContains(t, he, "NotFound")
		require.Equal(t, codes.NotFound, status.Code(ae))
	}
	for _, method := range []string{"GetInvocation", "GetTarget"} {
		r := req
		if method == "GetInvocation" {
			r = field(2, str(1, id))
		}
		want, we := call(app, ui+method, r)
		got, ge := call(second, ui+method, r)
		require.Equal(t, status.Code(we), status.Code(ge))
		if exists {
			if method == "GetInvocation" {
				require.Equal(t, first(t, first(t, want, 2), 20), first(t, first(t, got, 2), 20))
				require.Equal(t, id, string(first(t, first(t, got, 2), 1)))
			} else {
				require.Equal(t, want, got)
			}
		}
	}
	// Trend counts and SQL target history are UI projections of the same row.
	expected := uint64(0)
	if exists {
		expected = 1
	}
	trend, te := call(app, ui+"GetTrend", msg(field(1, str(2, f.GroupID)), field(2, str(1, "fixture-user"))))
	require.NoError(t, te)
	var total uint64
	for _, row := range bytesFields(t, trend, 2) {
		total += varintField(t, row, 3)
	}
	require.Equal(t, expected, total)
	tj := rpc(t, f, "GetTrend", map[string]any{"query": map[string]any{"user": "fixture-user"}})
	var jt uint64
	for _, row := range objects(t, tj, "trendStat") {
		v := row.(map[string]any)["totalNumBuilds"]
		require.Equal(t, "1", v)
		jt++
	}
	require.Equal(t, expected, jt)
	history, he2 := call(app, ui+"GetTargetHistory", msg(field(1, str(2, f.GroupID)), field(2, str(3, "https://example.invalid/conformance"))))
	require.NoError(t, he2)
	hj := rpc(t, f, "GetTargetHistory", map[string]any{"query": map[string]any{"repoUrl": "https://example.invalid/conformance"}})
	foundHistory := false
	for _, target := range bytesFields(t, history, 2) {
		for _, st := range bytesFields(t, target, 3) {
			if string(first(t, st, 1)) == id {
				foundHistory = true
			}
		}
	}
	require.Equal(t, exists, foundHistory, "target history invocation membership")
	require.Len(t, objects(t, hj, "invocationTargets"), int(expected))
	// Public API has no ACL field; its valid projections must remain readable to
	// both organization clients, then become NotFound when the SQL row is gone.
	for _, method := range []string{"GetLog", "GetTarget", "GetAction"} {
		for _, labelled := range []bool{false, true} {
			selector := str(1, id)
			js := map[string]any{"invocationId": id}
			if labelled && method != "GetLog" {
				n := protowire.Number(4)
				key := "label"
				if method == "GetAction" {
					n = 5
					key = "targetLabel"
				}
				selector = msg(selector, str(n, "//sample:test"))
				js[key] = "//sample:test"
			}
			r := field(1, selector)
			got, err := call(app, "/api.v1.ApiService/"+method, r)
			code, j := api(t, f, method, map[string]any{"selector": js})
			other, oe := call(second, "/api.v1.ApiService/"+method, r)
			require.Equal(t, status.Code(err), status.Code(oe))
			require.ElementsMatch(t, bytesFields(t, got, 1), bytesFields(t, other, 1))
			if exists {
				require.NoError(t, err)
				require.Equal(t, 200, code)
				if method == "GetLog" {
					require.Contains(t, string(first(t, first(t, got, 1), 3)), "management state machine")
					require.Contains(t, j["log"].(map[string]any)["contents"], "management state machine")
				} else {
					require.NotEmpty(t, bytesFields(t, got, 1))
					if method == "GetAction" {
						require.Contains(t, string(got), "test.log")
						require.Contains(t, string(got), harness.Digest([]byte("invocation-associated test output\n")).Hash)
					}
					key := "target"
					if method == "GetAction" {
						key = "action"
					}
					require.NotEmpty(t, objects(t, j, key))
				}
			} else {
				require.Equal(t, codes.NotFound, status.Code(err), method)
				require.NotEqual(t, 200, code)
				require.Contains(t, j["error"], "NotFound")
			}
		}
	}
	// Invocation-associated test.log is uploaded via the proxy by BES seeding.
	// Cache storage is content-addressed: invocation deletion preserves bytes.
	data := []byte("invocation-associated test output\n")
	digest := harness.Digest(data)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		got, err := f.Clients(component).Read(ctx, "", digest)
		cancel()
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
	for _, route := range []string{"download", "view"} {
		uri := fmt.Sprintf("bytestream://%s/blobs/%s/%d", strings.TrimPrefix(f.Endpoint(harness.App).InternalGRPC, "grpc://"), digest.Hash, digest.SizeBytes)
		req, err := http.NewRequest("GET", f.Endpoint(harness.App).HTTP+"/file/"+route+"?"+url.Values{"invocation_id": {id}, "bytestream_url": {uri}, "filename": {"test.log"}}.Encode(), nil)
		require.NoError(t, err)
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, string(body))
		require.Equal(t, data, body)
	}
	// UI raw event and log downloads, including the inline view route.
	for _, artifact := range []string{"raw_json", "raw_proto", "buildlog"} {
		for _, route := range []string{"download", "view"} {
			u := f.Endpoint(harness.App).HTTP + "/file/" + route + "?" + url.Values{"invocation_id": {id}, "artifact": {artifact}, "attempt": {"1"}}.Encode()
			r, err := http.NewRequest("GET", u, nil)
			require.NoError(t, err)
			r.Header.Set("x-buildbuddy-api-key", f.APIKey)
			resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(r)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			if exists {
				require.Equal(t, 200, resp.StatusCode, string(body))
				require.NotEmpty(t, body)
				if artifact == "buildlog" {
					require.Contains(t, string(body), "management state machine")
				}
			} else {
				// Known released-image status mapping divergence; isolated bug target requires 404.
				require.Equal(t, 500, resp.StatusCode, string(body))
				require.Contains(t, string(body), "Internal")
			}
		}
	}
	// Server streaming is a separate transport path from unary log reads.
	for _, c := range []*harness.Clients{app, anon} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		stream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, ui+"GetEventLog", grpc.ForceCodec(rawCodec{}))
		require.NoError(t, err)
		require.NoError(t, stream.SendMsg(msg(str(2, id), num(5, 1))))
		require.NoError(t, stream.CloseSend())
		var out []byte
		err = stream.RecvMsg(&out)
		if !exists {
			require.Equal(t, codes.NotFound, status.Code(err))
		} else if c == anon && !public {
			require.Equal(t, codes.Unauthenticated, status.Code(err))
		} else {
			require.NoError(t, err)
			require.Contains(t, string(first(t, out, 4)), "management state machine")
			for i := 0; i < 100; i++ {
				err = stream.RecvMsg(&out)
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
			}
			require.Equal(t, io.EOF, err)
		}
		cancel()
	}
	cli, err := exec.LookPath("bb")
	require.NoError(t, err)
	for _, extra := range [][]string{{"--errors"}, {"//sample:test"}, {"--test_filter=Sample"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		args := append([]string{"view", id, "--target=grpc://" + f.Endpoint(harness.App).GRPC}, extra...)
		cmd := exec.CommandContext(ctx, cli, args...)
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + t.TempDir(), "BUILDBUDDY_API_KEY=" + f.APIKey}
		out, err := cmd.CombinedOutput()
		cancel()
		if exists {
			require.NoError(t, err, string(out))
		} else {
			require.Error(t, err, string(out))
			if len(extra) == 1 && extra[0] == "//sample:test" {
				require.Contains(t, string(out), "target //sample:test not found in invocation "+id)
			} else if extra[0] == "--test_filter=Sample" {
				require.Contains(t, string(out), "invocation not found")
			} else {
				require.Contains(t, string(out), "NotFound")
			}
		}
	}
}
