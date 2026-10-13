package iprules_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Standards-expecting isolated repro: excluded from normal CI with the bug tag.
// No proxy, OLAP, or CLI is involved. App rule cache TTL stays at its default.
var signedAppRepro = flag.Bool("ip_rules_signed_app", false, "Enable signed app identity and also reproduce cache downloads")

func TestArtifactDownloadsEnforceIPRules(t *testing.T) {
	opts := harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--auth.ip_rules.enable=true", "--auth.trust_xforwardedfor_header=true", "--auth.api_key_group_cache_ttl=1s"}}}
	if *signedAppRepro {
		opts.Args[harness.App] = append(opts.Args[harness.App], "--app.client_identity.key=fixture-download-secret", "--app.client_identity.client=app", "--app.client_identity.origin=fixture")
	}
	f := harness.Start(t, opts)
	f.Web.Client.Transport = forwarded{f.Web.Client.Transport}
	if _, err := f.Web.RPC("UpdateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "name": "Download policy fixture", "urlIdentifier": "download-policy-fixture"}); err != nil {
		t.Fatal(err)
	}
	key := f.Web.CreateAPIKey(t, f.GroupID, "download-admin", []string{"ORG_ADMIN", "CACHE_WRITE"})
	client := harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	adminCtx := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", "192.0.2.10")
	id := seedInvocation(t, client, adminCtx)
	payload := []byte("private CAS download marker")
	digest, err := client.Upload(adminCtx, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := []string{"raw_json", "buildlog"}
	if *signedAppRepro {
		artifacts = append(artifacts, "cache")
	}
	call := func(method string, extra []byte) {
		var out []byte
		err := client.Conn.Invoke(adminCtx, "/buildbuddy.service.BuildBuddyService/"+method, append(field(1, field(2, []byte(f.GroupID))), extra...), &out, grpc.ForceCodec(wireCodec{}))
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func(ip, artifact string) (int, []byte) {
		original := f.Web.Client.Transport
		f.Web.Client.Transport = auditForwarded{original.(forwarded).base, ip}
		defer func() { f.Web.Client.Transport = original }()
		address := f.Web.URL + "/file/download?invocation_id=" + id + "&attempt=1&artifact=" + artifact
		if artifact == "cache" {
			address = f.Web.URL + "/file/download?bytestream_url=" + url.QueryEscape("bytestream://"+url.User(f.APIKey).String()+"@app:1985/"+harness.ResourceName("", digest, false))
		}
		rq, _ := http.NewRequestWithContext(ctx, "GET", address, nil)
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
		return rsp.StatusCode, b
	}
	for _, a := range artifacts {
		code, b := read("198.51.100.20", a)
		marker := []byte(strings.TrimSpace(logMarker))
		if a == "cache" {
			marker = payload
		}
		if a == "raw_json" {
			marker = []byte(`"command":"build"`)
		}
		if code != 200 || !bytes.Contains(b, marker) {
			t.Fatalf("baseline %s status=%d body=%q", a, code, b)
		}
	}
	call("AddIPRule", field(2, rule{CIDR: "192.0.2.10/32", Description: "admin anchor"}.wire()))
	call("SetIPRulesConfig", []byte{0x10, 1})
	deniedCtx := metadata.AppendToOutgoingContext(ctx, "x-forwarded-for", "198.51.100.20")
	start := time.Now()
	deadline := start.Add(10 * time.Second)
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
	t.Logf("CAS revoked after %s; checking artifacts for another 6 seconds", time.Since(start))
	leaks := map[string]int{}
	end := time.Now().Add(6 * time.Second)
	for {
		original := f.Web.Client.Transport
		f.Web.Client.Transport = auditForwarded{original.(forwarded).base, "198.51.100.20"}
		user, ue := f.Web.RPC("GetUser", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		_, le := f.Web.RPC("GetEventLogChunk", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id})
		f.Web.Client.Transport = original
		selected, _ := user["selectedGroup"].(map[string]any)
		if ue != nil || selected["access"] != "DENIED_BY_IP_RULES" || le == nil || !strings.Contains(le.Error(), "HTTP 403:") {
			t.Fatalf("denial controls failed: user=%v err=%v logErr=%v", user, ue, le)
		}
		for _, a := range artifacts {
			code, b := read("198.51.100.20", a)
			marker := []byte(strings.TrimSpace(logMarker))
			if a == "cache" {
				marker = payload
			}
			if a == "raw_json" {
				marker = []byte(`"command":"build"`)
			}
			if code == 200 && bytes.Contains(b, marker) {
				leaks[a]++
			} else if code != http.StatusForbidden {
				t.Fatalf("unexpected %s response: status=%d body=%q", a, code, b)
			}
		}
		if time.Now().After(end) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	for _, a := range artifacts {
		if leaks[a] > 0 {
			t.Errorf("%s: revoked IP received HTTP 200 with private content in %d samples over %s; CAS=PermissionDenied, GetUser=DENIED_BY_IP_RULES, GetEventLogChunk=HTTP 403", a, leaks[a], time.Since(start))
		}
	}
	t.Log(fmt.Sprintf("fixture-local invocation=%s", id))
}
