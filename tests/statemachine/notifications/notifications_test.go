package notifications

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/nacl/box"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
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
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"enable_secret_service": true}, "keystore": harness.Config{"master_key_uri": "local-insecure-kms://master", "local_insecure_kms_directory": "/fixture-kms"}}}, DockerArgs: map[harness.Component][]string{harness.App: {"--volume", dir + ":/fixture-kms:ro"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	keyValue := f.Web.CreateAPIKey(t, f.GroupID, "notifications", []string{"CACHE_WRITE", "ORG_ADMIN", "SEND_NOTIFICATION"})
	c := harness.Dial(t, f.Endpoint(harness.App).GRPC, keyValue)
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
	reference := map[string]snapshot{}
	for _, id := range ids {
		var ui map[string]any
		require.Eventually(t, func() bool {
			res, e := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
			ui = res
			rows, ok := res["invocation"].([]any)
			return e == nil && ok && len(rows) == 1
		}, 30*time.Second, 100*time.Millisecond)
		b := rpc(t, ctx, c, "GetInvocation", field(2, field(1, []byte(id))))
		inv := fields(t, b, 2)
		require.Len(t, inv, 1)
		require.Equal(t, id, string(fields(t, inv[0], 1)[0]))
		reference[id] = snapshot{normalizeWire(t, inv[0]), normalizeUI(ui)}
	}
	checkInvocations := func() {
		for _, id := range ids {
			require.Equal(t, reference[id].Wire, normalizeWire(t, fields(t, rpc(t, ctx, c, "GetInvocation", field(2, field(1, []byte(id)))), 2)[0]))
			require.Equal(t, reference[id].UI, normalizeUI(web("GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": id}})))
		}
	}
	pub := fields(t, rpc(t, ctx, c, "GetPublicKey", nil), 2)
	require.Len(t, pub, 1)
	pubBytes, err := base64.StdEncoding.DecodeString(string(fields(t, pub[0], 2)[0]))
	require.NoError(t, err)
	require.Len(t, pubBytes, 32)
	var pk [32]byte
	copy(pk[:], pubBytes)
	secretName := "NOTIFICATION_WEBHOOK"
	checkSecrets := func(present bool) {
		secrets := fields(t, rpc(t, ctx, c, "ListSecrets", nil), 2)
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
	checkSecrets(false)
	for generation := 0; generation < 2; generation++ {
		path := fmt.Sprintf("/channel-%d", generation)
		url := fmt.Sprintf("http://%s:%d%s", sinkIP, ln.Addr().(*net.TCPAddr).Port, path)
		encrypted, e := box.SealAnonymous(nil, []byte(url), &pk, rand.Reader)
		require.NoError(t, e)
		update := field(2, append(field(1, []byte(secretName)), field(2, []byte(base64.StdEncoding.EncodeToString(encrypted)))...))
		rpc(t, ctx, c, "UpdateSecret", update)
		checkSecrets(true)
		checkInvocations()
		// A repeated valid update preserves the selected configuration.
		rpc(t, ctx, c, "UpdateSecret", update)
		checkSecrets(true)
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
					require.Equal(t, "Nondeterminism detected", a["fields"].([]any)[0].(map[string]any)["title"])
					require.Equal(t, expected, a["actions"].([]any)[0].(map[string]any)["url"])
				case <-ctx.Done():
					t.Fatal("successful notification did not reach sink")
				}
				require.Empty(t, messages, "one successful single-channel request must deliver once")
				checkSecrets(true)
				checkInvocations()
			}
		}
	}
	rpc(t, ctx, c, "DeleteSecret", field(2, field(1, []byte(secretName))))
	checkSecrets(false)
	checkInvocations()
	rpc(t, ctx, c, "DeleteSecret", field(2, field(1, []byte(secretName))))
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
