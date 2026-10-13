package notifications

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/types/known/durationpb"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/nacl/box"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Stable wire fields are from released notification.proto, secrets.proto and
// invocation.proto. The codec avoids importing unreleased server code.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func rpc(t *testing.T, ctx context.Context, c *harness.Clients, name string, req []byte) []byte {
	t.Helper()
	var out []byte
	require.NoError(t, c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+name, &req, &out, grpc.ForceCodec(wireCodec{})))
	return out
}
func fields(t *testing.T, b []byte, n protowire.Number) [][]byte {
	t.Helper()
	var result [][]byte
	for len(b) > 0 {
		num, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		b = b[k:]
		size := protowire.ConsumeFieldValue(num, typ, b)
		require.GreaterOrEqual(t, size, 0)
		if num == n && typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(b)
			result = append(result, append([]byte(nil), v...))
		}
		b = b[size:]
	}
	return result
}

// Capability order is unspecified (the server projects a set).
func normalizeUI(m map[string]any) map[string]any {
	for _, v := range m["invocation"].([]any) {
		row := v.(map[string]any)
		if caps, ok := row["createdWithCapabilities"].([]any); ok {
			sort.Slice(caps, func(i, j int) bool { return caps[i].(string) < caps[j].(string) })
		}
	}
	return m
}
func normalizeWire(t *testing.T, b []byte) []byte {
	var out []byte
	var caps []uint64
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		size := protowire.ConsumeFieldValue(n, typ, b[k:])
		require.GreaterOrEqual(t, size, 0)
		if n == 27 {
			if typ == protowire.BytesType {
				v, _ := protowire.ConsumeBytes(b[k:])
				for len(v) > 0 {
					x, l := protowire.ConsumeVarint(v)
					require.Greater(t, l, 0)
					caps = append(caps, x)
					v = v[l:]
				}
			} else {
				x, _ := protowire.ConsumeVarint(b[k:])
				caps = append(caps, x)
			}
		} else {
			out = append(out, b[:k+size]...)
		}
		b = b[k+size:]
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	for _, x := range caps {
		out = append(out, number(27, x)...)
	}
	return out
}
func TestNotificationStateMachine(t *testing.T) {
	// Listen only for disposable local webhook deliveries. The container reaches
	// the runner through its private interface address, never a public recipient.
	var sinkIP string
	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, e := iface.Addrs()
		require.NoError(t, e)
		for _, addr := range addrs {
			ip, _, e := net.ParseCIDR(addr.String())
			if e == nil && ip.To4() != nil && ip.IsPrivate() {
				sinkIP = ip.String()
				break
			}
		}
		if sinkIP != "" {
			break
		}
	}
	require.NotEmpty(t, sinkIP, "runner must have a fixture-reachable private IPv4 address")
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	type delivery struct {
		Path string
		Body map[string]any
	}
	messages := make(chan delivery, 32)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		messages <- delivery{r.URL.Path, body}
		w.Write([]byte("ok"))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0755))
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "master"), key, 0644))
	olap := startOLAP(t, sinkIP)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"enable_secret_service": true, "audit_logs_enabled": true}, "olap_database": harness.Config{"data_source": olap}, "keystore": harness.Config{"master_key_uri": "local-insecure-kms://master", "local_insecure_kms_directory": "/fixture-kms"}}}, DockerArgs: map[harness.Component][]string{harness.App: {"--volume", dir + ":/fixture-kms:ro"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	keyValue := f.Web.CreateAPIKey(t, f.GroupID, "notifications", []string{"CACHE_WRITE", "ORG_ADMIN", "SEND_NOTIFICATION"})
	c := harness.Dial(t, f.Endpoint(harness.App).GRPC, keyValue)
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "notifications-second", []string{"CACHE_WRITE", "ORG_ADMIN", "SEND_NOTIFICATION"})
	second := harness.Dial(t, f.Endpoint(harness.App).GRPC, secondKey)
	proxy := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, secondKey)
	for _, name := range []string{"ListSecrets", "GetPublicKey"} {
		var req, out []byte
		err := proxy.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+name, &req, &out, grpc.ForceCodec(wireCodec{}))
		require.Equal(t, codes.Unimplemented, status.Code(err), "proxy does not register secret service")
	}
	cliDir := t.TempDir()
	init := exec.CommandContext(ctx, "git", "init", "--quiet", cliDir)
	init.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + cliDir, "GIT_CONFIG_NOSYSTEM=1"}
	require.NoError(t, init.Run())
	configure := exec.CommandContext(ctx, "git", "-C", cliDir, "config", "buildbuddy.api-key", secondKey)
	configure.Env = init.Env
	require.NoError(t, configure.Run())
	cli := func() string {
		cmd := exec.CommandContext(ctx, "bb", "secrets", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "list")
		cmd.Dir = cliDir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + cliDir, "GIT_CONFIG_NOSYSTEM=1"}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	web := func(name string, req map[string]any) map[string]any {
		req["requestContext"] = f.Web.RequestContext(f.GroupID)
		res, e := f.Web.RPC(name, req)
		require.NoError(t, e)
		return res
	}
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, id := range ids {
		publishInvocation(t, ctx, c, id)
	}
	type snapshot struct {
		Wire []byte
		UI   map[string]any
	}
	// BES projections finalize asynchronously. Freeze only after two seconds
	// of unchanged reads, rather than taking independent unsettled snapshots.
	schema := invocationSchema(t, ctx, c)
	reference := map[string]snapshot{}
	for _, id := range ids {
		var ui map[string]any
		var previous map[string]any
		stableSince := time.Now()
		require.Eventually(t, func() bool {
			ui = normalizeUI(web("GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}}))
			rows, ok := ui["invocation"].([]any)
			if !ok || len(rows) != 1 {
				stableSince = time.Now()
				return false
			}
			if !reflect.DeepEqual(previous, ui) {
				previous = ui
				stableSince = time.Now()
				return false
			}
			return time.Since(stableSince) >= 2*time.Second
		}, 30*time.Second, 100*time.Millisecond)
		b := rpc(t, ctx, c, "GetInvocation", field(2, field(1, []byte(id))))
		inv := fields(t, b, 2)
		require.Len(t, inv, 1)
		require.Equal(t, id, string(fields(t, inv[0], 1)[0]))
		row := ui["invocation"].([]any)[0].(map[string]any)
		require.Equal(t, id, row["invocationId"])
		require.Equal(t, "build", row["command"])
		require.Equal(t, "build", string(fields(t, inv[0], 7)[0]))
		require.Equal(t, uint64(1), wireNumber(t, inv[0], 3), "model expects successful invocation")
		require.Equal(t, true, row["success"])
		equalInvocationProjections(t, schema, inv[0], ui)
		reference[id] = snapshot{normalizeWire(t, inv[0]), normalizeUI(ui)}
	}
	checkInvocations := func() {
		for _, id := range ids {
			require.Equal(t, reference[id].Wire, normalizeWire(t, fields(t, rpc(t, ctx, c, "GetInvocation", field(2, field(1, []byte(id)))), 2)[0]))
			ui := normalizeUI(web("GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}}))
			require.Equal(t, reference[id].UI, ui)
			equalInvocationProjections(t, schema, fields(t, rpc(t, ctx, second, "GetInvocation", field(2, field(1, []byte(id)))), 2)[0], ui)
		}
	}
	pub := fields(t, rpc(t, ctx, c, "GetPublicKey", nil), 2)
	require.Len(t, pub, 1)
	require.NotEmpty(t, fields(t, pub[0], 1))
	pubBytes, err := base64.StdEncoding.DecodeString(string(fields(t, pub[0], 2)[0]))
	require.NoError(t, err)
	require.Len(t, pubBytes, 32)
	uiKey := web("GetPublicKey", map[string]any{})["publicKey"].(map[string]any)
	require.Equal(t, string(fields(t, pub[0], 1)[0]), uiKey["id"])
	require.Equal(t, string(fields(t, pub[0], 2)[0]), uiKey["value"])
	t.Log("HTTP public key ID/value agree with gRPC")
	var pk [32]byte
	copy(pk[:], pubBytes)
	secretName := "NOTIFICATION_WEBHOOK"
	expectedValue := ""
	auditActions := []string{}
	auditStart := time.Now().Add(-time.Minute)
	checkAudit := func() {
		req := append(field(3, number(1, uint64(auditStart.Unix()))), field(4, number(1, uint64(time.Now().Add(time.Hour).Unix())))...)
		response := rpc(t, ctx, second, "GetAuditLogs", req)
		got := []string{}
		for _, entry := range fields(t, response, 2) {
			resources := fields(t, entry, 3)
			require.Len(t, resources, 1)
			if wireNumber(t, resources[0], 1) != 4 {
				continue
			}
			if string(fields(t, resources[0], 2)[0]) != secretName {
				continue
			}
			action := ""
			if wireNumber(t, entry, 4) == 1 {
				action = "CREATE"
			} else {
				require.Equal(t, uint64(4), wireNumber(t, entry, 4))
				action = "UPDATE"
			}
			got = append(got, action)
			request := fields(t, entry, 5)[0]
			api := fields(t, request, 1)[0]
			secret := fields(t, fields(t, api, 12)[0], 2)[0]
			require.Empty(t, fields(t, secret, 2), "audit must redact encrypted values")
		}
		require.Equal(t, auditActions, got)
		ui := web("GetAuditLogs", map[string]any{"timestampAfter": auditStart.UTC().Format(time.RFC3339Nano), "timestampBefore": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
		uiActions := []string{}
		if entries, ok := ui["entries"].([]any); ok {
			for _, raw := range entries {
				entry := raw.(map[string]any)
				resource := entry["resource"].(map[string]any)
				if resource["id"] != secretName {
					continue
				}
				uiActions = append(uiActions, entry["action"].(string))
				request := entry["request"].(map[string]any)["apiRequest"].(map[string]any)["updateSecret"].(map[string]any)["secret"].(map[string]any)
				require.Equal(t, secretName, request["name"])
				require.Empty(t, request["value"])
			}
		}
		require.Equal(t, auditActions, uiActions)
	}
	// Consume from the other API key after every update/delete, not only after
	// selected generations. Absence is also checked through the real consumer.
	consumeNotification := func(present bool) {
		event := append(field(1, []byte(ids[0])), field(1, []byte(ids[1]))...)
		req := append(field(2, event), field(3, field(2, field(1, []byte(secretName))))...)
		started := time.Now()
		if present {
			rpc(t, ctx, second, "SendNotification", req)
			select {
			case msg := <-messages:
				require.Equal(t, expectedValue[strings.LastIndex(expectedValue, "/"):], msg.Path)
				attachments := msg.Body["attachments"].([]any)
				require.Len(t, attachments, 1)
				a := attachments[0].(map[string]any)
				require.Equal(t, "#ad1411", a["color"])
				require.Equal(t, []any{map[string]any{"title": "Nondeterminism detected", "value": "Running the same command twice produced different outputs.", "short": false}}, a["fields"])
				require.Equal(t, []any{map[string]any{"type": "button", "text": "See on BuildBuddy", "url": "http://app:8080/compare/" + ids[0] + "..." + ids[1], "style": ""}}, a["actions"])
			case <-ctx.Done():
				t.Fatal("notification consumer did not deliver")
			}
			t.Logf("notification delivery observation upper bound=%s", time.Since(started))
		} else {
			var out []byte
			err := second.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/SendNotification", &req, &out, grpc.ForceCodec(wireCodec{}))
			require.Equal(t, codes.NotFound, status.Code(err))
		}
		select {
		case msg := <-messages:
			t.Fatalf("unexpected late notification: %s", msg.Path)
		case <-time.After(500 * time.Millisecond):
		}
	}

	checkSecrets := func(present bool) {
		start := time.Now()
		consumeNotification(present)
		// Poll the independent listing surfaces against the expected model, then
		// verify every consumer twice with an observation interval.
		wantName := ""
		if present {
			wantName = secretName
		}
		require.Eventually(t, func() bool {
			for _, client := range []*harness.Clients{c, second} {
				rows := fields(t, rpc(t, ctx, client, "ListSecrets", nil), 2)
				if !present {
					if len(rows) != 0 {
						return false
					}
				} else {
					if len(rows) != 1 || string(fields(t, rows[0], 1)[0]) != secretName || len(fields(t, rows[0], 2)) != 0 {
						return false
					}
				}
			}
			ui := web("ListSecrets", map[string]any{})
			if present {
				rows, ok := ui["secret"].([]any)
				if !ok || len(rows) != 1 || rows[0].(map[string]any)["name"] != secretName {
					return false
				}
			} else {
				if rows, ok := ui["secret"].([]any); ok && len(rows) != 0 {
					return false
				}
			}
			return cli() == wantName
		}, 5*time.Second, 50*time.Millisecond)
		observe := func() {
			checkAudit()
			secrets := fields(t, rpc(t, ctx, c, "ListSecrets", nil), 2)
			require.Equal(t, secrets, fields(t, rpc(t, ctx, second, "ListSecrets", nil), 2))
			require.Equal(t, pub, fields(t, rpc(t, ctx, second, "GetPublicKey", nil), 2))
			require.Equal(t, uiKey, web("GetPublicKey", map[string]any{})["publicKey"])
			expectedName := ""
			if present {
				expectedName = secretName
			}
			require.Equal(t, expectedName, cli())
			checkExecutionSecret(t, ctx, second, expectedValue)

			ui := web("ListSecrets", map[string]any{})
			if !present {
				require.Empty(t, secrets)
				require.Empty(t, ui["secret"])
				return
			}
			require.Len(t, secrets, 1)
			require.Equal(t, secretName, string(fields(t, secrets[0], 1)[0]))
			require.Empty(t, fields(t, secrets[0], 2))
			rows := ui["secret"].([]any)
			require.Len(t, rows, 1)
			require.Equal(t, secretName, rows[0].(map[string]any)["name"])
			require.Empty(t, rows[0].(map[string]any)["value"])
		}
		observe()
		elapsed := time.Since(start)
		time.Sleep(250 * time.Millisecond)
		observe()
		t.Logf("model convergence observation upper bound=%s; stable recheck after 250ms", elapsed)
	}
	checkSecrets(false)
	for generation := 0; generation < 4; generation++ {
		if generation == 3 {
			rpc(t, ctx, second, "DeleteSecret", field(2, field(1, []byte(secretName))))
			expectedValue = ""
			checkSecrets(false)
			checkInvocations()
		}
		path := fmt.Sprintf("/channel-%d", generation)
		url := fmt.Sprintf("http://%s:%d%s", sinkIP, ln.Addr().(*net.TCPAddr).Port, path)
		expectedValue = url
		encrypted, e := box.SealAnonymous(nil, []byte(url), &pk, rand.Reader)
		require.NoError(t, e)
		update := field(2, append(field(1, []byte(secretName)), field(2, []byte(base64.StdEncoding.EncodeToString(encrypted)))...))
		writer := c
		if generation%2 == 1 {
			writer = second
		}
		rpc(t, ctx, writer, "UpdateSecret", update)
		action := "UPDATE"
		if generation == 0 || generation == 3 {
			action = "CREATE"
		}
		auditActions = append(auditActions, action)
		checkSecrets(true)
		checkInvocations()
		// A repeated valid update preserves the selected configuration.
		rpc(t, ctx, c, "UpdateSecret", update)
		auditActions = append(auditActions, "UPDATE")
		checkSecrets(true)
		checkInvocations()
		for _, parent := range []string{"", ids[2]} {
			for repeat := 0; repeat < 2; repeat++ {
				event := append(field(1, []byte(ids[0])), field(1, []byte(ids[1]))...)
				expected := "http://app:8080/compare/" + ids[0] + "..." + ids[1]
				if parent != "" {
					event = append(event, field(2, []byte(parent))...)
					expected = "http://app:8080/invocation/" + parent
				}
				req := append(field(2, event), field(3, field(2, field(1, []byte(secretName))))...)
				rpc(t, ctx, c, "SendNotification", req)
				select {
				case msg := <-messages:
					require.Equal(t, path, msg.Path)
					attachments := msg.Body["attachments"].([]any)
					require.Len(t, attachments, 1)
					a := attachments[0].(map[string]any)
					require.Equal(t, "#ad1411", a["color"])
					require.Equal(t, []any{map[string]any{"title": "Nondeterminism detected", "value": "Running the same command twice produced different outputs.", "short": false}}, a["fields"])
					require.Equal(t, []any{map[string]any{"type": "button", "text": "See on BuildBuddy", "url": expected, "style": ""}}, a["actions"])
					require.Equal(t, expected, a["actions"].([]any)[0].(map[string]any)["url"])
				case <-ctx.Done():
					t.Fatal("successful notification did not reach sink")
				}
				select {
				case msg := <-messages:
					t.Fatalf("duplicate delivery: %s", msg.Path)
				case <-time.After(500 * time.Millisecond):
				}
				checkSecrets(true)
				checkInvocations()
			}
		}
	}
	rpc(t, ctx, c, "DeleteSecret", field(2, field(1, []byte(secretName))))
	expectedValue = ""
	checkSecrets(false)
	event := append(field(1, []byte(ids[0])), field(1, []byte(ids[1]))...)
	req := append(field(2, event), field(3, field(2, field(1, []byte(secretName))))...)
	var out []byte
	err = c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/SendNotification", &req, &out, grpc.ForceCodec(wireCodec{}))
	require.Equal(t, codes.NotFound, status.Code(err), "deleted secret must not remain consumable")
	select {
	case msg := <-messages:
		t.Fatalf("deleted secret delivered to %s", msg.Path)
	case <-time.After(500 * time.Millisecond):
	}
	t.Log("deleted secret rejected by notification consumer, no delivery")
	checkInvocations()
	rpc(t, ctx, c, "DeleteSecret", field(2, field(1, []byte(secretName))))
	expectedValue = ""
	checkSecrets(false)
	checkInvocations()
}

func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func number(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func publishInvocation(t *testing.T, ctx context.Context, c *harness.Clients, id string) {
	t.Helper()
	started := field(1, []byte(id))
	started = append(started, number(2, uint64(time.Now().UnixMilli()))...)
	started = append(started, field(3, []byte("8.0.0"))...)
	started = append(started, field(4, []byte("--color=no"))...)
	started = append(started, field(5, []byte("build"))...)
	first := append(field(1, field(3, nil)), field(2, field(9, nil))...)
	first = append(first, field(5, started)...)
	// Supply an actual finish time, as a real Bazel producer does.
	finishPayload := append(field(3, append(field(1, []byte("SUCCESS")), number(2, 0)...)), number(2, uint64(time.Now().UnixMilli()))...)
	finished := append(field(1, field(9, nil)), field(14, finishPayload)...)
	finished = append(finished, number(20, 1)...)
	s, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	streamID := &bespb.StreamId{BuildId: id, InvocationId: id, Component: bespb.StreamId_TOOL}
	for i, event := range [][]byte{first, finished} {
		require.NoError(t, s.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: streamID, SequenceNumber: int64(i + 1), Event: &bespb.BuildEvent{EventTime: timestamppb.Now(), Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: event}}}}}))
	}
	require.NoError(t, s.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: streamID, SequenceNumber: 3, Event: &bespb.BuildEvent{EventTime: timestamppb.Now(), Event: &bespb.BuildEvent_ComponentStreamFinished{ComponentStreamFinished: &bespb.BuildEvent_BuildComponentStreamFinished{Type: bespb.BuildEvent_BuildComponentStreamFinished_FINISHED}}}}}))
	// The released server acknowledges only after EOF and finalization.
	require.NoError(t, s.CloseSend())
	for sequence := int64(1); sequence <= 3; sequence++ {
		ack, err := s.Recv()
		require.NoError(t, err)
		require.Equal(t, sequence, ack.SequenceNumber)
	}
	_, err = s.Recv()
	require.Equal(t, io.EOF, err)
}

// Each action is uncached, including after deletion. The environment is resolved
// by the app and consumed by a released executor, independently of ListSecrets.
func checkExecutionSecret(t *testing.T, ctx context.Context, c *harness.Clients, want string) {
	t.Helper()
	instance := "notification-secrets"
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	command, err := c.UploadProto(ctx, instance, &repb.Command{
		Arguments: []string{"/bin/sh", "-c", "printf '%s' \"${NOTIFICATION_WEBHOOK-}\""},
		Platform:  &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}, {Name: "include-secrets", Value: "true"}}},
	})
	require.NoError(t, err)
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: command, InputRootDigest: root, DoNotCache: true, Timeout: durationpb.New(20 * time.Second)})
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true})
	require.NoError(t, err)
	for {
		op, err := stream.Recv()
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		response := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(response))
		require.Zero(t, response.GetStatus().GetCode(), "%v", response.GetStatus())
		require.Zero(t, response.GetResult().GetExitCode())
		got := response.GetResult().GetStdoutRaw()
		if d := response.GetResult().GetStdoutDigest(); d != nil {
			got, err = c.Read(ctx, instance, d)
			require.NoError(t, err)
		}
		require.Equal(t, want, string(got))
		return
	}
}

func wireNumber(t *testing.T, b []byte, wanted protowire.Number) uint64 {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		b = b[k:]
		size := protowire.ConsumeFieldValue(n, typ, b)
		require.GreaterOrEqual(t, size, 0)
		if n == wanted {
			require.Equal(t, protowire.VarintType, typ)
			v, k := protowire.ConsumeVarint(b)
			require.Greater(t, k, 0)
			return v
		}
		b = b[size:]
	}
	return 0
}
