package iprules_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
)

const logMarker = "ip-rule private invocation log marker"

// Wire encoding mirrors proto/iprules.proto and proto/context.proto without
// pulling generated app protos into the shared dependency scaffold.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}

// xff sets X-Forwarded-For on every request made through the web client.
type xff struct {
	base http.RoundTripper
	ip   *string
}

func (x xff) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", *x.ip)
	return x.base.RoundTrip(r)
}

func seedInvocation(t *testing.T, c *harness.Clients, ctx context.Context) string {
	t.Helper()
	id := fmt.Sprintf("%08x-0000-4000-8000-%012x", uint32(time.Now().UnixNano()), uint64(time.Now().UnixNano())&0xffffffffffff)
	started := append(field(1, field(3, nil)), field(5, append(field(4, []byte("--bes_upload_mode=wait_for_upload_complete")), field(5, []byte("build"))...))...)
	progress := append(field(1, field(2, nil)), field(3, field(1, []byte(logMarker+"\n")))...)
	finished := append(append(field(1, field(9, nil)), field(14, field(3, field(1, []byte("SUCCESS"))))...), 0xa0, 0x01, 0x01)
	stream, err := c.BES.PublishBuildToolEventStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, payload := range [][]byte{started, progress, finished} {
		ev := &bespb.BuildEvent{Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: payload}}}
		sid := &bespb.StreamId{InvocationId: id, BuildId: id, Component: bespb.StreamId_TOOL}
		if err := stream.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: sid, SequenceNumber: int64(i + 1), Event: ev}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	acks := 0
	for {
		if _, err := stream.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		acks++
	}
	if acks != 3 {
		t.Fatalf("BES ACK count=%d", acks)
	}
	return id
}

// Known divergence (tag `bug`): once an IP rule revokes a client, gRPC and /rpc
// deny it but /file/download keeps serving private invocation artifacts.
func TestArtifactDownloadsEnforceIPRules(t *testing.T) {
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App},
		Args: map[harness.Component][]string{harness.App: {
			"--auth.ip_rules.enable=true",
			"--auth.trust_xforwardedfor_header=true",
			"--auth.api_key_group_cache_ttl=1s",
		}},
	})
	const adminIP, clientIP = "192.0.2.10", "198.51.100.20"
	ip := adminIP
	f.Web.Client.Transport = xff{f.Web.Client.Transport, &ip}
	if _, err := f.Web.RPC("UpdateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "name": "Download policy fixture", "urlIdentifier": "download-policy-fixture"}); err != nil {
		t.Fatal(err)
	}
	key := f.Web.CreateAPIKey(t, f.GroupID, "download-admin", []string{"ORG_ADMIN", "CACHE_WRITE"})
	client := harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	adminCtx := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", adminIP)
	id := seedInvocation(t, client, adminCtx)

	download := func(artifact string) (int, string) {
		rq, _ := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?invocation_id="+id+"&attempt=1&artifact="+artifact, nil)
		rq.Header.Set("x-buildbuddy-api-key", f.APIKey)
		rsp, err := f.Web.Client.Do(rq)
		if err != nil {
			t.Fatal(err)
		}
		defer rsp.Body.Close()
		b, err := io.ReadAll(rsp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return rsp.StatusCode, string(b)
	}
	markers := map[string]string{"buildlog": logMarker, "raw_json": `"command":"build"`}

	// Baseline: the not-yet-revoked client can download both artifacts.
	ip = clientIP
	for a, m := range markers {
		if code, b := download(a); code != 200 || !strings.Contains(b, m) {
			t.Fatalf("baseline %s status=%d body=%q", a, code, b)
		}
	}

	// The one mutation: allow only the admin IP and enforce rules.
	ip = adminIP
	call := func(method string, extra []byte) {
		var out []byte
		req := append(field(1, field(2, []byte(f.GroupID))), extra...)
		if err := client.Conn.Invoke(adminCtx, "/buildbuddy.service.BuildBuddyService/"+method, req, &out, grpc.ForceCodec(wireCodec{})); err != nil {
			t.Fatal(err)
		}
	}
	call("AddIPRule", field(2, append(field(2, []byte(adminIP+"/32")), field(3, []byte("admin anchor"))...)))
	call("SetIPRulesConfig", []byte{0x10, 1})

	// Control: the enforcing surfaces deny the revoked IP.
	deniedCtx := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", clientIP)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := client.CAS.FindMissingBlobs(deniedCtx, &repb.FindMissingBlobsRequest{})
		if status.Code(err) == codes.PermissionDenied {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CAS never denied: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	ip = clientIP
	_, le := f.Web.RPC("GetEventLogChunk", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id})
	if le == nil || !strings.Contains(le.Error(), "HTTP 403:") {
		t.Fatalf("control: /rpc GetEventLogChunk should be 403, got %v", le)
	}

	// Divergence: /file/download should be 403 too.
	for a, m := range markers {
		code, b := download(a)
		if code == 200 && bytes.Contains([]byte(b), []byte(m)) {
			t.Errorf("%s: revoked IP got HTTP 200 with private content; CAS=PermissionDenied, GetEventLogChunk=HTTP 403", a)
		} else if code != http.StatusForbidden {
			t.Errorf("%s: unexpected status %d", a, code)
		}
	}
}
