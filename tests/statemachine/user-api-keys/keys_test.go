package userkeys

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
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestUserKeyStateMachine(t *testing.T) {
	for _, path := range []string{"ui-grpc", "public-grpc", "public-http"} {
		t.Run(path, func(t *testing.T) { runMachine(t, path) })
	}
}

func runMachine(t *testing.T, creationPath string) {
	startedAt := time.Now().Add(-time.Second)
	creationTimes := map[string]time.Time{}
	sslCfg, sslDocker, rootCA := certificateConfig(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, DockerArgs: map[harness.Component][]string{harness.App: sslDocker}, Config: map[harness.Component]harness.Config{harness.App: {"ssl": sslCfg, "app": harness.Config{"user_owned_keys_enabled": true}, "auth": harness.Config{"api_key_group_cache_ttl": "1s"}}, harness.CacheProxy: {"auth": harness.Config{"remote": harness.Config{"jwt_cache_ttl": "1s"}}}}})
	_, err := f.Web.RPC("UpdateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "id": f.GroupID, "name": "user-key-state-machine", "urlIdentifier": "user-key-state-machine", "userOwnedKeysEnabled": true})
	must(t, err)
	user, _ := f.Web.UserID["id"].(string)
	if user == "" {
		t.Fatal("missing fixture user ID")
	}
	admin := f.Web.CreateAPIKey(t, f.GroupID, "local-admin", []string{"ORG_ADMIN", "CACHE_WRITE"})
	c := harness.Dial(t, f.Endpoint(harness.App).GRPC, admin)
	rc := str(str(nil, 1, string(str(nil, 1, user))), 2, f.GroupID)
	base := str(nil, 1, string(rc))
	webRC := f.Web.RequestContext(f.GroupID)
	anchorResponse, e := rpc(c, ui+"CreateUserApiKey", num(str(str(base, 6, user), 3, "setup-reader"), 4, 1))
	must(t, e)
	anchor := fromWire(parse(anchorResponse)[2][0])
	setupClient := harness.Dial(t, f.Endpoint(harness.App).GRPC, anchor.Value)
	model := map[string]key{anchor.ID: anchor}
	check := func() {
		b, e := rpc(c, ui+"GetUserApiKeys", str(base, 3, user))
		must(t, e)
		listed := map[string]key{}
		for _, v := range parse(b)[2] {
			k := fromWire(v)
			if _, duplicate := listed[k.ID]; duplicate {
				t.Fatal("duplicate gRPC list ID")
			}
			listed[k.ID] = k
		}
		res, e := f.Web.RPC("GetUserApiKeys", map[string]any{"requestContext": webRC, "userId": user})
		must(t, e)
		web := map[string]key{}
		if a, ok := res["apiKey"].([]any); ok {
			for _, v := range a {
				k := fromJSON(v.(map[string]any))
				if _, duplicate := web[k.ID]; duplicate {
					t.Fatal("duplicate HTTP list ID")
				}
				web[k.ID] = k
			}
		}
		gb, ge := rpc(c, ui+"GetApiKeys", base)
		must(t, ge)
		for _, v := range parse(gb)[2] {
			if _, ok := model[fromWire(v).ID]; ok {
				t.Fatal("user key leaked into org-key list")
			}
		}
		gw, ge := f.Web.RPC("GetApiKeys", map[string]any{"requestContext": webRC})
		must(t, ge)
		if a, ok := gw["apiKey"].([]any); ok {
			for _, v := range a {
				if _, found := model[fromJSON(v.(map[string]any)).ID]; found {
					t.Fatal("user key leaked into web org-key list")
				}
			}
		}
		{
			res, e := f.Web.RPC("GetBazelConfig", map[string]any{"requestContext": webRC, "host": "app", "protocol": "http:", "includeCertificate": true})
			must(t, e)
			gb, e := rpc(setupClient, ui+"GetBazelConfig", num(str(str(base, 2, "app"), 3, "http:"), 4, 1))
			must(t, e)
			grpcSetup := map[string]key{}
			for _, credential := range parse(gb)[4] {
				raw := []byte(parse(credential).one(1))
				item := fromWire(raw)
				if _, owned := model[item.ID]; owned {
					if parse(raw).one(6) != "\x01" {
						t.Fatal("gRPC setup lost user ownership")
					}
					if _, duplicate := grpcSetup[item.ID]; duplicate {
						t.Fatal("duplicate setup ID")
					}
					grpcSetup[item.ID] = item
					cert := parse([]byte(parse(credential).one(2)))
					checkCertificate(t, rootCA, item.ID, cert.one(1), cert.one(2))
				}
			}
			setup := map[string]key{}
			if a, ok := res["credential"].([]any); ok {
				for _, v := range a {
					m := v.(map[string]any)["apiKey"].(map[string]any)
					k := fromJSON(m)
					if _, owned := model[k.ID]; owned {
						if m["userOwned"] != true {
							t.Fatal("setup lost user ownership")
						}
						setup[k.ID] = k
						cert := v.(map[string]any)["certificate"].(map[string]any)
						checkCertificate(t, rootCA, k.ID, cert["cert"].(string), cert["key"].(string))
					}
				}
			}
			for id, k := range model {
				if k.Alive {
					// Setup credentials intentionally omit capabilities and secrets.
					k.Caps = nil
					compare(t, k, setup[id], false)
					compare(t, k, grpcSetup[id], false)
				} else if _, ok := grpcSetup[id]; ok {
					t.Fatal("deleted key still in grpc setup")
				} else if _, ok := setup[id]; ok {
					t.Fatal("deleted key still in setup credentials")
				}
			}
		}
		alive := 0
		for id, k := range model {
			if k.Alive {
				alive++
				compare(t, k, listed[id], false)
				compare(t, k, web[id], false)
				metadata := listed[id]
				if metadata.CreatedAt.Before(startedAt) || metadata.CreatedAt.After(time.Now().Add(time.Second)) {
					t.Fatalf("creation timestamp outside mutation window: %s", metadata.CreatedAt)
				}
				if !metadata.CreatedAt.Equal(web[id].CreatedAt) || metadata.CreatedBy != web[id].CreatedBy {
					t.Fatal("creation metadata differs between grpc and HTTP")
				}
				if metadata.CreatedBy != "" {
					t.Fatal("org-key mutation attributed to a user")
				}
				if original, ok := creationTimes[id]; ok {
					if !original.Equal(metadata.CreatedAt) {
						t.Fatal("update changed creation timestamp")
					}
				} else {
					creationTimes[id] = metadata.CreatedAt
				}

			} else {
				if _, ok := listed[id]; ok {
					t.Fatal("deleted key still listed")
				}
				if _, ok := web[id]; ok {
					t.Fatal("deleted key still web-listed")
				}
			}
			{
				b, e := rpc(c, ui+"GetApiKey", str(base, 2, id))
				w, we := f.Web.RPC("GetApiKey", map[string]any{"requestContext": webRC, "apiKeyId": id})
				if k.Alive {
					must(t, e)
					must(t, we)
					compare(t, k, fromWire(parse(b)[2][0]), true)
					compare(t, k, fromJSON(w["apiKey"].(map[string]any)), true)
				} else if status.Code(e) != codes.NotFound || (we == nil || !strings.Contains(we.Error(), "NotFound")) {
					t.Fatalf("missed GetApiKey deletion surface grpc=%v http=%v", e, we)
				}
			}
			b, e := rpc(c, ui+"GetUserApiKey", str(base, 2, id))
			wr, we := f.Web.RPC("GetUserApiKey", map[string]any{"requestContext": webRC, "apiKeyId": id})
			if k.Alive {
				must(t, e)
				must(t, we)
				compare(t, k, fromWire(parse(b)[2][0]), true)
				compare(t, k, fromJSON(wr["apiKey"].(map[string]any)), true)
				for _, method := range []string{"GetApiKey", "GetUserApiKey"} {
					cb, e := rpc(c, ui+method, num(str(base, 2, id), 3, 1))
					must(t, e)
					certFields := parse([]byte(parse([]byte(parse(cb).one(2))).one(8)))
					checkCertificate(t, rootCA, id, certFields.one(1), certFields.one(2))
					cw, e := f.Web.RPC(method, map[string]any{"requestContext": webRC, "apiKeyId": id, "includeCertificate": true})
					must(t, e)
					certificate := cw["apiKey"].(map[string]any)["certificate"].(map[string]any)
					checkCertificate(t, rootCA, id, certificate["cert"].(string), certificate["key"].(string))
				}

			} else {
				if status.Code(e) != codes.NotFound || (we == nil || !strings.Contains(we.Error(), "NotFound")) {
					t.Fatalf("deleted key read: grpc=%v web=%v", e, we)
				}
			}
		}
		if len(listed) != alive || len(web) != alive {
			t.Fatalf("list sizes grpc=%d web=%d model=%d", len(listed), len(web), alive)
		}
	}

	cliDir := t.TempDir()
	cliPath := filepath.Join(cliDir, "bb")
	cliHTTP := &http.Client{Timeout: 2 * time.Minute}
	binaryResponse, e := cliHTTP.Get("https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64")
	must(t, e)
	binary, e := io.ReadAll(io.LimitReader(binaryResponse.Body, 100<<20))
	binaryResponse.Body.Close()
	must(t, e)
	if binaryResponse.StatusCode != 200 || fmt.Sprintf("%x", sha256.Sum256(binary)) != "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9" {
		t.Fatal("CLI download/checksum mismatch")
	}
	must(t, os.WriteFile(cliPath, binary, 0755))
	cliRepo := filepath.Join(cliDir, "repo")
	must(t, os.Mkdir(cliRepo, 0755))
	runCLICommand := func(program string, args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.Dir = cliRepo
		// Do not inherit dispatch credentials or worker git configuration.
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + cliDir, "GIT_CONFIG_GLOBAL=/dev/null"}
		return cmd.Run()
	}
	must(t, runCLICommand("git", "init", "--quiet"))
	cliCheck := func(k key, live bool) {
		must(t, runCLICommand("git", "config", "buildbuddy.api-key", k.Value))
		e := runCLICommand(cliPath, "login", "--check", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--url="+f.Endpoint(harness.App).HTTP)
		if live {
			must(t, e)
		} else {
			exit, ok := e.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 {
				t.Fatalf("deleted CLI login: %v", e)
			}
		}
	}
	probeSequence := 0
	capabilityCheck := func(client *harness.Clients, component harness.Component, k key) {
		wantCAS, wantAC := len(k.Caps) > 0, false
		for _, cap := range k.Caps {
			if cap == 1 {
				wantAC = true
			}
		}
		readers := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
		started := time.Now()
		stable := 0
		deadline := started.Add(20 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			probeSequence++
			payload := []byte(fmt.Sprintf("%s-%s-%d", k.ID, component, probeSequence))
			d := harness.Digest(payload)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			r, e := client.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: payload}}})
			must(t, e)
			if len(r.Responses) != 1 || r.Responses[0].GetStatus().GetCode() != 0 {
				t.Fatal("valid CAS write failed")
			}
			streamPayload := append([]byte("stream-"), payload...)
			streamDigest, e := client.Upload(ctx, "", streamPayload)
			must(t, e)
			ad := harness.Digest(append([]byte("action-"), payload...))
			_, e = client.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: ad, ActionResult: &repb.ActionResult{StdoutRaw: payload}})
			must(t, e)
			converged := true
			md := str(nil, 1, string(str(str(nil, 1, "bazel"), 2, "7.0.0")))
			capCtx := metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(md))
			advertised, e := client.Capabilities.GetCapabilities(capCtx, &repb.GetCapabilitiesRequest{})
			must(t, e)
			if advertised.GetCacheCapabilities().GetActionCacheUpdateCapabilities().GetUpdateEnabled() != wantAC {
				converged = false
			}

			for _, reader := range readers {
				missing, e := reader.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
				must(t, e)
				if (len(missing.MissingBlobDigests) == 0) != wantCAS {
					converged = false
				}
				streamed, e := reader.Read(ctx, "", streamDigest)
				if wantCAS {
					if e != nil || !bytes.Equal(streamed, streamPayload) {
						converged = false
					}
				} else if status.Code(e) != codes.NotFound {
					converged = false
				}
				batch, e := reader.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}})
				must(t, e)
				if len(batch.Responses) != 1 {
					t.Fatal("invalid batch read response")
				}
				if wantCAS {
					if batch.Responses[0].GetStatus().GetCode() != 0 || !bytes.Equal(batch.Responses[0].Data, payload) {
						converged = false
					}
				} else if batch.Responses[0].GetStatus().GetCode() != int32(codes.NotFound) {
					converged = false
				}
				ar, e := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: ad, InlineStdout: true})
				if wantAC {
					if e != nil || !bytes.Equal(ar.GetStdoutRaw(), payload) {
						converged = false
					}
				} else if status.Code(e) != codes.NotFound {
					converged = false
				}
			}
			cancel()
			if component == harness.App {
				jwtResponse, e := rpc(client, "/auth.AuthService/Authenticate", nil)
				must(t, e)
				parts := strings.Split(parse(jwtResponse).one(1), ".")
				if len(parts) != 3 {
					t.Fatal("invalid auth JWT")
				}
				body, e := base64.RawURLEncoding.DecodeString(parts[1])
				must(t, e)
				var claims map[string]any
				must(t, json.Unmarshal(body, &claims))
				if claims["user_id"] != user || claims["group_id"] != f.GroupID || claims["api_key_id"] != k.ID {
					t.Fatalf("JWT identity mismatch: %v", claims)
				}
				var caps []int
				if values, ok := claims["capabilities"].([]any); ok {
					for _, v := range values {
						caps = append(caps, int(v.(float64)))
					}
				}
				sort.Ints(caps)
				if !reflect.DeepEqual(caps, k.Caps) {
					converged = false
				}
			}
			if converged {
				stable++
			} else {
				stable = 0
			}
			if stable >= 3 {
				t.Logf("%s capabilities %v converged and stable in %s", component, k.Caps, time.Since(started))
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s capabilities %v failed to converge on app and proxy", component, k.Caps)
			}
			<-ticker.C
		}
	}

	auth := func(k key, live bool) {
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			client := harness.Dial(t, f.Endpoint(component).GRPC, k.Value)
			if live && component == harness.App {
				b, e := rpc(client, ui+"GetUser", base)
				must(t, e)
				display := parse(parse(b)[1][0])
				if parse(display[1][0]).one(1) != user {
					t.Fatal("key authenticated as wrong owner")
				}
			}
			started := time.Now()
			stable := 0
			deadline := started.Add(20 * time.Second)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, e := client.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{harness.Digest([]byte("auth-probe"))}})
				cancel()
				if live && e == nil || !live && (status.Code(e) == codes.Unauthenticated || status.Code(e) == codes.PermissionDenied) {
					stable++
				} else {
					stable = 0
				}
				if stable >= 3 {
					t.Logf("%s live=%v converged and stable in %s", component, live, time.Since(started))
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s live=%v auth failed to converge: %v", component, live, e)
				}
				<-ticker.C
			}
			if !live && component == harness.App {
				_, e := rpc(client, ui+"GetUser", base)
				if status.Code(e) != codes.Unauthenticated && status.Code(e) != codes.PermissionDenied {
					t.Fatalf("deleted GetUser accepted: %v", e)
				}
				_, e = rpc(client, "/auth.AuthService/Authenticate", nil)
				if status.Code(e) != codes.Unauthenticated && status.Code(e) != codes.PermissionDenied {
					t.Fatalf("deleted Authenticate accepted: %v", e)
				}
			}
			if component == harness.App {
				cliCheck(k, live)
			}
			if live {
				capabilityCheck(client, component, k)
			}
		}
	}
	checkAll := func() {
		check()
		ids := make([]string, 0, len(model))
		for id := range model {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			auth(model[id], model[id].Alive)
		}
	}
	checkAll()
	for _, path := range []string{creationPath} {
		// Same-label repeated creates are valid distinct keys, not idempotent creates.
		for repeat := 0; repeat < 2; repeat++ {
			label := path
			var k key
			if path == "ui-grpc" {
				req := str(str(base, 6, user), 3, label)
				req = num(req, 4, 1)
				b, e := rpc(c, ui+"CreateUserApiKey", req)
				must(t, e)
				k = fromWire(parse(b)[2][0])
			} else if path == "public-grpc" {
				b, e := rpc(c, "/api.v1.ApiService/CreateUserApiKey", str(str(nil, 1, user), 2, label))
				must(t, e)
				p := parse(parse(b)[1][0])
				k = key{ID: p.one(1), Value: p.one(2), Label: p.one(3), Caps: []int{1}, Alive: true}
			} else {
				body, _ := json.Marshal(map[string]any{"user_id": user, "label": label})
				req, e := http.NewRequest("POST", f.Endpoint(harness.App).HTTP+"/api/v1/CreateUserApiKey", bytes.NewReader(body))
				must(t, e)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-buildbuddy-api-key", admin)
				client := &http.Client{Timeout: 10 * time.Second}
				r, e := client.Do(req)
				must(t, e)
				data, e := io.ReadAll(r.Body)
				r.Body.Close()
				must(t, e)
				if r.StatusCode != 200 {
					t.Fatalf("public HTTP %d: %s", r.StatusCode, data)
				}
				var out struct {
					ApiKey struct {
						ID    string `json:"apiKeyId"`
						Value string `json:"value"`
						Label string `json:"label"`
					} `json:"apiKey"`
				}
				must(t, json.Unmarshal(data, &out))
				k = key{ID: out.ApiKey.ID, Value: out.ApiKey.Value, Label: out.ApiKey.Label, Caps: []int{1}, Alive: true}
			}
			if k.ID == "" || k.Value == "" {
				t.Fatal("empty create response")
			}
			if _, ok := model[k.ID]; ok {
				t.Fatal("repeated create reused ID")
			}
			expectedCaps := []int{1}
			if k.Label != label || !reflect.DeepEqual(k.Caps, expectedCaps) {
				t.Fatal("create response did not match requested label or expected capabilities")
			}
			model[k.ID] = k
			checkAll()
		}
	}
	ids := make([]string, 0, len(model))
	for id := range model {
		if id != anchor.ID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		k := model[id]
		for _, caps := range [][]int{{4}, {1}, {}} {
			k.Label = "updated-" + id
			k.Caps = caps
			if len(caps) == 0 {
				k.Label = ""
				k.Caps = nil
			}
			req := str(str(base, 2, id), 3, k.Label)
			for _, cap := range caps {
				req = num(req, 4, uint64(cap))
			}
			for repeat := 0; repeat < 2; repeat++ {
				_, e := rpc(c, ui+"UpdateUserApiKey", req)
				must(t, e)
				model[id] = k
				checkAll()
			}
		}
		_, e := rpc(c, ui+"DeleteUserApiKey", str(base, 2, id))
		must(t, e)
		k.Alive = false
		model[id] = k
		checkAll()
		recreatedResponse, e := rpc(c, ui+"CreateUserApiKey", num(str(str(base, 6, user), 3, k.Label), 4, 1))
		must(t, e)
		recreated := fromWire(parse(recreatedResponse)[2][0])
		if recreated.ID == id || recreated.Value == k.Value {
			t.Fatal("re-create reused identity or secret")
		}
		model[recreated.ID] = recreated
		checkAll()
		_, e = rpc(c, ui+"DeleteUserApiKey", str(base, 2, recreated.ID))
		must(t, e)
		recreated.Alive = false
		model[recreated.ID] = recreated
		checkAll()
	}
	t.Logf("validated %d keys across three creation transports", len(model))
}
