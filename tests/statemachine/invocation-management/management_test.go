package invocation_management_test

import (
	"bytes"
	"context"
	"encoding/base64"
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

// rawCodec sends valid release-schema protobufs without generated app bindings.
type rawCodec struct{}

func (rawCodec) Name() string                  { return "proto" }
func (rawCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (rawCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func bytesFields(t *testing.T, b []byte, n protowire.Number) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		k, typ, l := protowire.ConsumeTag(b)
		require.Positive(t, l)
		b = b[l:]
		z := protowire.ConsumeFieldValue(k, typ, b)
		require.Positive(t, z)
		if k == n && typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(b)
			out = append(out, v)
		}
		b = b[z:]
	}
	return out
}
func first(t *testing.T, b []byte, n protowire.Number) []byte {
	v := bytesFields(t, b, n)
	if len(v) == 0 {
		return nil
	}
	return v[0]
}
func call(c *harness.Clients, method string, b []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out []byte
	err := c.Conn.Invoke(ctx, method, b, &out, grpc.ForceCodec(rawCodec{}))
	return out, err
}

const ui = "/buildbuddy.service.BuildBuddyService/"

func TestManagementStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	rpc(t, f, "UpdateGroup", map[string]any{"id": f.GroupID, "name": "management", "urlIdentifier": "management", "sharingEnabled": true})
	id := upload(t, f, []string{"management state machine\n"}, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	data := []byte("independent CAS object survives invocation deletion")
	d, err := proxy.Upload(ctx, "", data)
	require.NoError(t, err)
	// The reference model records only state controlled by management mutations.
	model := struct{ exists, public bool }{true, false}
	lookup := field(2, str(1, id))
	ownerReq := str(2, id)
	// Preserve ownership and owner/group permissions when changing public read.
	raw, err := call(app, ui+"GetInvocation", lookup)
	require.NoError(t, err)
	originalACL := first(t, first(t, raw, 2), 20)
	aclWire := msg(field(1, first(t, originalACL, 1)), str(2, f.GroupID), field(3, first(t, originalACL, 3)), field(4, first(t, originalACL, 4)))
	anonymous := harness.Dial(t, f.Endpoint(harness.App).GRPC, "")
	check := func() {
		t.Helper()
		for _, surface := range []string{"GetInvocation", "GetInvocationOwner"} {
			req := lookup
			jsonReq := map[string]any{"lookup": map[string]any{"invocationId": id}}
			if surface == "GetInvocationOwner" {
				req = ownerReq
				jsonReq = map[string]any{"invocationId": id}
			}
			b, e := call(app, ui+surface, req)
			jsonReq["requestContext"] = f.Web.RequestContext(f.GroupID)
			j, he := f.Web.RPC(surface, jsonReq)
			if surface == "GetInvocationOwner" {
				require.Equal(t, codes.PermissionDenied, status.Code(e))
				require.ErrorContains(t, he, "PermissionDenied")
				continue
			}
			if !model.exists {
				require.Equal(t, codes.NotFound, status.Code(e), surface)
				require.ErrorContains(t, he, "NotFound", surface)
				continue
			}
			require.NoError(t, e)
			require.NoError(t, he)
			{
				inv := objects(t, j, "invocation")
				require.Len(t, inv, 1)
				a := inv[0].(map[string]any)["acl"].(map[string]any)
				require.Equal(t, f.GroupID, a["groupId"])
				actualACL := first(t, first(t, b, 2), 20)
				for _, n := range []protowire.Number{1, 2, 3, 4} {
					require.True(t, bytes.Equal(first(t, originalACL, n), first(t, actualACL, n)), "ownership and non-public permissions must be preserved")
				}
				require.Equal(t, id, inv[0].(map[string]any)["invocationId"])
				require.Equal(t, "COMPLETE_INVOCATION_STATUS", inv[0].(map[string]any)["invocationStatus"])
				require.Equal(t, model.public, func() bool { p, _ := a["othersPermissions"].(map[string]any); return p["read"] == true }())
				wantPublic := []byte(nil)
				if model.public {
					wantPublic = num(1, 1)
				}
				require.True(t, bytes.Equal(wantPublic, first(t, first(t, first(t, b, 2), 20), 5)), "public permission wire")
			}
		}
		search := rpc(t, f, "SearchInvocation", map[string]any{"query": map[string]any{"groupId": f.GroupID, "user": "fixture-user", "host": "fixture-host"}})
		b, e := call(app, ui+"SearchInvocation", field(2, msg(str(1, "fixture-user"), str(2, "fixture-host"), str(3, f.GroupID))))
		require.NoError(t, e)
		expected := 0
		if model.exists {
			expected = 1
		}
		require.Len(t, objects(t, search, "invocation"), expected)
		require.Len(t, bytesFields(t, b, 2), expected)
		if model.exists {
			require.Equal(t, id, string(first(t, first(t, b, 2), 1)))
			require.Equal(t, id, objects(t, search, "invocation")[0].(map[string]any)["invocationId"])
		}
		stats := rpc(t, f, "GetInvocationStat", map[string]any{"aggregationType": "USER_AGGREGATION_TYPE", "query": map[string]any{"user": "fixture-user"}})
		sb, se := call(app, ui+"GetInvocationStat", msg(field(1, str(2, f.GroupID)), num(2, 1), field(4, str(1, "fixture-user"))))
		require.NoError(t, se)
		require.Len(t, objects(t, stats, "invocationStat"), expected)
		require.Len(t, bytesFields(t, sb, 2), expected)
		if model.exists {
			require.Equal(t, "1", objects(t, stats, "invocationStat")[0].(map[string]any)["totalNumBuilds"])
		}
		// bb view's supported build-log read must lose access with the invocation.
		lb, le := call(app, ui+"GetEventLogChunk", msg(str(2, id), num(5, 1)))
		lj, lhe := f.Web.RPC("GetEventLogChunk", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id, "type": "BUILD_LOG"})
		if lhe == nil {
			decoded, de := base64.StdEncoding.DecodeString(lj["buffer"].(string))
			require.NoError(t, de)
			require.Equal(t, first(t, lb, 4), decoded)
		}
		if model.exists {
			require.NoError(t, le)
			require.NoError(t, lhe)
			require.Contains(t, string(first(t, lb, 4)), "management state machine")
		} else {
			require.Error(t, le)
			require.Error(t, lhe)
		}
		apiReq := field(1, str(1, id))
		b, e = call(app, "/api.v1.ApiService/GetInvocation", apiReq)
		code, j := api(t, f, "GetInvocation", map[string]any{"selector": map[string]any{"invocationId": id}})
		if model.exists {
			require.NoError(t, e)
			require.Len(t, bytesFields(t, b, 1), 1)
			require.Equal(t, 200, code)
			require.Len(t, objects(t, j, "invocation"), 1)
			require.Equal(t, id, string(first(t, first(t, first(t, b, 1), 1), 1)))
			require.Equal(t, id, objects(t, j, "invocation")[0].(map[string]any)["id"].(map[string]any)["invocationId"])
		} else {
			require.NoError(t, e)
			require.Empty(t, bytesFields(t, b, 1))
			require.Equal(t, 200, code)
			require.Empty(t, objects(t, j, "invocation"))
		}
		_, e = call(anonymous, ui+"GetInvocation", lookup)
		if model.exists && model.public {
			require.NoError(t, e)
		} else {
			require.Error(t, e)
		}
		for _, c := range []*harness.Clients{app, proxy} {
			got, e := c.Read(ctx, "", d)
			require.NoError(t, e)
			require.Equal(t, data, got)
		}
	}
	check()
	// Exercise bb view only when the runner supplies the CLI; never inherit dispatch credentials.
	cliPath, cliErr := exec.LookPath("bb")
	if cliErr != nil {
		t.Log("runner has no bb CLI; its underlying log RPC is covered")
	}
	cliCheck := func() {
		if cliErr != nil {
			return
		}
		cc, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		cmd := exec.CommandContext(cc, cliPath, "view", id, "--target=grpc://"+f.Endpoint(harness.App).GRPC)
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + t.TempDir(), "BUILDBUDDY_API_KEY=" + f.APIKey}
		out, e := cmd.CombinedOutput()
		if model.exists {
			require.NoError(t, e, string(out))
			require.True(t, strings.Contains(string(out), "management state machine"), string(out))
		} else {
			require.Error(t, e, string(out))
		}
	}
	cliCheck()
	// Each update is repeated to establish idempotency, then reverted.
	for _, public := range []bool{true, true, false, false, true} {
		wire := append([]byte(nil), aclWire...)
		if public {
			wire = append(wire, field(5, num(1, 1))...)
		} else {
			wire = append(wire, field(5, nil)...)
		}
		_, err = call(app, ui+"UpdateInvocation", msg(str(2, id), field(3, wire)))
		require.NoError(t, err)
		model.public = public
		check()
		cliCheck()
	}
	_, err = call(app, ui+"DeleteInvocation", str(2, id))
	require.NoError(t, err)
	model.exists = false
	check()
	cliCheck()
	_, err = call(app, ui+"DeleteInvocation", str(2, id))
	require.Equal(t, codes.NotFound, status.Code(err))
	check()
	cliCheck()
}
