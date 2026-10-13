package githubstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
)

const repoURL = "https://github.com/fixture-owner/project"

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Wire encoding is deliberately limited to the released proto schemas named in
// README.md; no generated BuildBuddy source is linked into these image tests.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func str(n protowire.Number, v string) []byte {
	return protowire.AppendString(protowire.AppendTag(nil, n, protowire.BytesType), v)
}
func num(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func fields(b []byte, n protowire.Number) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		field, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("invalid response tag")
		}
		b = b[k:]
		var value []byte
		switch typ {
		case protowire.BytesType:
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				panic("invalid bytes")
			}
			value = v
			b = b[k:]
		case protowire.VarintType:
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				panic("invalid varint")
			}
			value = protowire.AppendVarint(nil, v)
			b = b[k:]
		default:
			k := protowire.ConsumeFieldValue(field, typ, b)
			if k < 0 {
				panic("invalid field")
			}
			b = b[k:]
		}
		if field == n {
			out = append(out, value)
		}
	}
	return out
}
func first(b []byte, n protowire.Number) []byte {
	v := fields(b, n)
	if len(v) == 0 {
		return nil
	}
	return v[0]
}
func boolean(b []byte, n protowire.Number) bool { return bytes.Equal(first(b, n), []byte{1}) }

type model struct{ user, group, installation, repo, status, defaults bool }
type driver struct {
	strict bool
	ctx    context.Context
	peer   *grpc.ClientConn
	keys   []*grpc.ClientConn
	writes int
	jwt    string
	t      *testing.T
	f      *harness.Fixture
	conn   *grpc.ClientConn
	rc     []byte
	m      model
}

func (d *driver) request(method string, extra []byte) []byte {
	n := protowire.Number(1)
	if method == "GetUser" || method == "GetGroup" {
		n = 2
	}
	return join(str(n, string(d.rc)), extra)
}
func (d *driver) grpc(method string, extra []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(d.context(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-jwt", d.jwt)
	req := d.request(method, extra)
	var res []byte
	err := d.conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, &req, &res, grpc.ForceCodec(wireCodec{}))
	return res, err
}
func (d *driver) http(method string, extra []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(d.context(), "POST", d.f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(d.request(method, extra)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/protobuf")
	res, err := d.f.Web.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("%s HTTP %d: %s", method, res.StatusCode, b)
	}
	return b, err
}
func (d *driver) read(method string, extra []byte) ([]byte, error) {
	g, err := d.grpc(method, extra)
	if err != nil {
		return nil, fmt.Errorf("%s gRPC: %w", method, err)
	}
	h, err := d.http(method, extra)
	if err != nil {
		return nil, err
	}
	// Capabilities depend on the authentication surface; compare link state below.
	if method != "GetUser" && !bytes.Equal(g, h) {
		return nil, fmt.Errorf("%s gRPC/HTTP differ: %x / %x", method, g, h)
	}
	if method == "GetUser" {
		if boolean(g, 7) != boolean(h, 7) || !reflect.DeepEqual(groupLinks(g), groupLinks(h)) {
			return nil, fmt.Errorf("GetUser link flags differ between gRPC and HTTP")
		}
	}
	return g, nil
}
func groupLinks(b []byte) map[string]bool {
	m := map[string]bool{}
	for _, g := range fields(b, 2) {
		m[string(first(g, 1))] = boolean(g, 4)
	}
	return m
}

// JSON is the browser protolet representation; protobuf HTTP is checked too.
func (d *driver) jsonCheck() error {
	rc := d.f.Web.RequestContext(d.f.GroupID)
	for _, method := range []string{"GetUser", "GetGroup", "GetGitHubAppInstallations", "GetLinkedGitHubRepos", "GetAccessibleGitHubRepos", "GetGithubUserInstallations", "GetGithubUser", "GetRepos"} {
		req := map[string]any{"requestContext": rc}
		switch method {
		case "GetGroup":
			req["urlIdentifier"] = "github-link-state"
		case "GetAccessibleGitHubRepos":
			req["installationId"] = "101"
		case "GetRepos":
			req["gitProvider"] = "GITHUB"
		}
		result, err := d.jsonRPC(method, req)
		denied := (!d.m.user && (method == "GetAccessibleGitHubRepos" || method == "GetGithubUserInstallations" || method == "GetGithubUser")) || (!d.m.group && method == "GetRepos")
		if denied {
			if !authDenial(err) {
				return fmt.Errorf("%s JSON unexpected denial: %v", method, err)
			}
			continue
		}
		if err != nil {
			return err
		}
		truth := func(m map[string]any, k string) bool { v, _ := m[k].(bool); return v }
		list := func(k string) []any { v, _ := result[k].([]any); return v }
		switch method {
		case "GetUser":
			if truth(result, "githubLinked") != d.m.user {
				return fmt.Errorf("GetUser JSON user link mismatch")
			}
			found := false
			for _, row := range list("userGroup") {
				g := row.(map[string]any)
				if g["id"] == d.f.GroupID {
					found = true
					if truth(g, "githubLinked") != d.m.group {
						return fmt.Errorf("GetUser JSON group link mismatch")
					}
				}
			}
			if !found {
				return fmt.Errorf("GetUser JSON missing org")
			}
		case "GetGroup":
			if result["id"] != d.f.GroupID {
				return fmt.Errorf("GetGroup JSON wrong org")
			}
		case "GetGitHubAppInstallations":
			rows := list("installations")
			want := 0
			if d.m.installation {
				want = 1
			}
			if len(rows) != want {
				return fmt.Errorf("JSON installation count mismatch")
			}
			if want == 1 {
				r := rows[0].(map[string]any)
				if r["owner"] != "fixture-owner" || r["groupId"] != d.f.GroupID || r["installationId"] != "101" || r["appId"] != "7" || truth(r, "reportCommitStatusesForCiBuilds") != d.m.status {
					return fmt.Errorf("JSON installation mismatch: %v", r)
				}
			}
		case "GetLinkedGitHubRepos":
			if len(list("repoUrls")) != 0 {
				return fmt.Errorf("deprecated JSON repoUrls unexpectedly populated")
			}
			rows := list("repos")
			want := 0
			if d.m.repo {
				want = 1
			}
			if len(rows) != want {
				return fmt.Errorf("JSON linked repository count mismatch")
			}
			if want == 1 {
				r := rows[0].(map[string]any)
				if r["repoUrl"] != repoURL || truth(r, "useDefaultWorkflowConfig") != d.m.defaults || truth(r, "readWriteAppInstalled") != (!d.strict || d.m.installation) {
					return fmt.Errorf("JSON linked repository mismatch: %v", r)
				}
			}
		case "GetAccessibleGitHubRepos":
			if !reflect.DeepEqual(list("repoUrls"), []any{repoURL}) {
				return fmt.Errorf("JSON accessible repositories mismatch")
			}
		case "GetGithubUserInstallations":
			rows := list("installations")
			if len(rows) != 1 || rows[0].(map[string]any)["id"] != "101" {
				return fmt.Errorf("JSON user installations mismatch")
			}
		case "GetGithubUser":
			if result["login"] != "fixture-user" {
				return fmt.Errorf("JSON GitHub user mismatch")
			}
		case "GetRepos":
			rows := list("repo")
			if len(rows) != 1 || rows[0].(map[string]any)["url"] != repoURL {
				return fmt.Errorf("JSON legacy repositories mismatch")
			}
		}
	}
	return nil
}
func (d *driver) check() error {
	u, err := d.read("GetUser", nil)
	if err != nil {
		return err
	}
	links := groupLinks(u)
	if links[d.f.Web.GroupID] {
		return fmt.Errorf("unrelated org GitHub link changed")
	}
	if boolean(u, 7) != d.m.user || links[d.f.GroupID] != d.m.group {
		return fmt.Errorf("GetUser links user=%v group=%v; model=%+v", boolean(u, 7), links, d.m)
	}
	g, err := d.read("GetGroup", str(1, "github-link-state"))
	if err != nil {
		return err
	}
	if string(first(g, 1)) != d.f.GroupID {
		return fmt.Errorf("GetGroup wrong ID")
	}
	ins, err := d.read("GetGitHubAppInstallations", nil)
	if err != nil {
		return err
	}
	rows := fields(ins, 2)
	want := 0
	if d.m.installation {
		want = 1
	}
	if len(rows) != want {
		return fmt.Errorf("installation count=%d want=%d", len(rows), want)
	}
	if want == 1 {
		r := rows[0]
		if !bytes.Equal(first(r, 1), []byte{101}) || string(first(r, 2)) != "fixture-owner" || string(first(r, 3)) != d.f.GroupID || !bytes.Equal(first(r, 4), []byte{7}) || boolean(r, 5) != d.m.status {
			return fmt.Errorf("installation does not match model: %x", r)
		}
	}
	repos, err := d.read("GetLinkedGitHubRepos", nil)
	if err != nil {
		return err
	}
	if len(fields(repos, 2)) != 0 {
		return fmt.Errorf("deprecated repo_urls unexpectedly populated: %x", repos)
	}
	rows = fields(repos, 3)
	want = 0
	if d.m.repo {
		want = 1
	}
	if len(rows) != want {
		return fmt.Errorf("linked repo count=%d want=%d", len(rows), want)
	}
	if want == 1 {
		r := rows[0]
		if string(first(r, 1)) != repoURL || boolean(r, 2) != d.m.defaults || boolean(r, 3) != (!d.strict || d.m.installation) {
			j, err := d.jsonRPC("GetLinkedGitHubRepos", map[string]any{"requestContext": d.f.Web.RequestContext(d.f.GroupID)})
			if err != nil {
				return err
			}
			return fmt.Errorf("linked repository does not match model: %x; HTTP protobuf identical; JSON=%v", r, j)
		}
	}
	if d.m.user {
		gu, err := d.read("GetGithubUser", nil)
		if err != nil {
			return err
		}
		if string(first(gu, 3)) != "fixture-user" {
			return fmt.Errorf("GitHub user login mismatch")
		}
		accessible, err := d.read("GetAccessibleGitHubRepos", num(2, 101))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(fields(accessible, 2), [][]byte{[]byte(repoURL)}) {
			return fmt.Errorf("accessible repositories changed on local link mutation")
		}
		userIns, err := d.read("GetGithubUserInstallations", nil)
		if err != nil {
			return err
		}
		rows = fields(userIns, 2)
		if len(rows) != 1 || !bytes.Equal(first(rows[0], 1), []byte{101}) || string(first(rows[0], 2)) != "fixture-owner" {
			return fmt.Errorf("GitHub user installation changed on local mutation")
		}
	} else {
		for _, method := range []string{"GetGithubUser", "GetGithubUserInstallations", "GetAccessibleGitHubRepos"} {
			extra := []byte(nil)
			if method == "GetAccessibleGitHubRepos" {
				extra = num(2, 101)
			}
			_, gerr := d.grpc(method, extra)
			_, herr := d.http(method, extra)
			if !authDenial(gerr) || !authDenial(herr) {
				return fmt.Errorf("%s denial mismatch: grpc=%v http=%v", method, gerr, herr)
			}
		}
	}
	if d.m.group {
		r, err := d.read("GetRepos", num(2, 1))
		if err != nil {
			return err
		}
		rows = fields(r, 2)
		if len(rows) != 1 || string(first(rows[0], 1)) != repoURL {
			return fmt.Errorf("legacy GetRepos does not match GitHub authorization")
		}
	} else {
		_, gerr := d.grpc("GetRepos", num(2, 1))
		_, herr := d.http("GetRepos", num(2, 1))
		if !authDenial(gerr) || !authDenial(herr) {
			return fmt.Errorf("GetRepos denial mismatch: grpc=%v http=%v", gerr, herr)
		}
	}
	if err := d.auditReaders(); err != nil {
		return err
	}
	if err := d.contentReaders(); err != nil {
		return err
	}
	if err := d.workflowReaders(); err != nil {
		return err
	}
	if err := d.keyReaders(); err != nil {
		return err
	}
	return d.jsonCheck()
}
func (d *driver) converge(step string) {
	d.t.Helper()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	d.ctx = ctx
	defer func() { d.ctx = nil }()
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	lastText := ""
	for {
		if ctx.Err() != nil {
			d.t.Fatalf("%s failed to converge: %v", step, last)
		}
		snapshotErr := d.check()
		if ctx.Err() != nil {
			d.t.Fatalf("%s failed to converge within %s: %v", step, time.Since(start), last)
		}
		last = snapshotErr
		if last != nil && last.Error() != lastText {
			lastText = last.Error()
			d.t.Logf("%s snapshot: %v", step, last)
		}
		if last == nil {
			elapsed := time.Since(start)
			for i := 0; i < 3; i++ {
				select {
				case <-ctx.Done():
					d.t.Fatal(ctx.Err())
				case <-ticker.C:
				}
				if err := d.check(); err != nil {
					d.t.Fatalf("%s unstable after %s: %v", step, elapsed, err)
				}
			}
			d.t.Logf("CONVERGENCE %s: %s; stable for %s (3 complete snapshots)", step, elapsed, time.Since(start)-elapsed)
			return
		}
		select {
		case <-ctx.Done():
			d.t.Fatalf("%s failed to converge: %v", step, last)
		case <-ticker.C:
		}
	}
}
func (d *driver) mutate(method string, extra []byte, update func()) {
	d.t.Helper()
	d.writes++
	if d.writes%2 == 0 {
		d.conn, d.peer = d.peer, d.conn
	}
	var res []byte
	var err error
	switch method {
	case "UpdateGitHubAppInstallation", "UpdateGitHubRepoSettings", "UnlinkGitHubRepo":
		req := d.request(method, extra)
		ctx, cancel := context.WithTimeout(d.context(), 5*time.Second)
		err = d.keys[d.writes%len(d.keys)].Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, &req, &res, grpc.ForceCodec(wireCodec{}))
		cancel()
	default:
		res, err = d.grpc(method, extra)
	}
	must(d.t, err)
	if method == "UnlinkGitHubAccount" && len(fields(res, 2)) != 0 {
		d.t.Fatalf("mutation warnings: %q", fields(res, 2))
	}
	update()
	d.converge(method)
}
func (d *driver) oauth(path string, group bool) {
	d.t.Helper()
	client := *d.f.Web.Client
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	query := url.Values{"redirect_url": {d.f.Web.URL}}
	if group {
		query.Set("group_id", d.f.GroupID)
	} else {
		query.Set("user_id", d.f.Web.UserID["id"].(string))
	}
	res, err := client.Get(d.f.Web.URL + path + "?" + query.Encode())
	must(d.t, err)
	res.Body.Close()
	location, err := res.Location()
	must(d.t, err)
	state := location.Query().Get("state")
	if state == "" {
		d.t.Fatalf("OAuth failed to redirect: %s", location)
	}
	res, err = client.Get(d.f.Web.URL + path + "?code=fixture-valid-code&state=" + url.QueryEscape(state))
	must(d.t, err)
	res.Body.Close()
	location, err = res.Location()
	must(d.t, err)
	if location.Query().Get("error") != "" {
		d.t.Fatalf("OAuth callback: %s", location)
	}
	if group {
		d.m.group = true
	} else {
		d.m.user = true
	}
	d.converge("OAuth link")
}
func TestGitHubLinkState(t *testing.T) { runStateMachine(t, false) }
func runStateMachine(t *testing.T, strict bool) {
	proxy, cert, key := githubMock(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"github": harness.Config{"jwt_key": "fixture-only-github-jwt", "client_id": "fixture-legacy", "client_secret": "fixture-secret", "app": harness.Config{"enabled": true, "id": "7", "client_id": "fixture-app", "client_secret": "fixture-secret", "public_link": "https://github.com/apps/fixture", "private_key": key, "webhook_secret": "fixture-webhook"}}}}, DockerArgs: map[harness.Component][]string{harness.App: {"--env", "HTTPS_PROXY=" + proxy, "--env", "SSL_CERT_FILE=/fixture-github-ca.pem", "--mount", "type=bind,src=" + cert + ",dst=/fixture-github-ca.pem,readonly"}}})
	// Use a real provisioned user and org. The supported gRPC JWT auth uses
	// the fixture signing secret and the same existing org-admin role as HTTP.
	res, err := f.Web.RPC("CreateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "name": "github-state", "urlIdentifier": "github-link-state"})
	must(t, err)
	f.GroupID = res["id"].(string)
	uid := f.Web.UserID["id"].(string)
	payload, err := json.Marshal(map[string]any{"user_id": uid, "group_id": f.GroupID, "allowed_groups": []string{f.GroupID}, "group_memberships": []any{map[string]any{"group_id": f.GroupID, "capabilities": []int{8}}}, "capabilities": []int{8}, "exp": time.Now().Add(time.Hour).Unix()})
	must(t, err)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte("fixture-only-hmac-key"))
	mac.Write([]byte(encoded))
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	d := &driver{strict: strict, t: t, f: f, conn: harness.Dial(t, f.Endpoint(harness.App).GRPC, "").Conn, jwt: token, rc: join(str(1, string(str(1, uid))), str(2, f.GroupID))}

	d.peer = harness.Dial(t, f.Endpoint(harness.App).GRPC, "").Conn
	for _, label := range []string{"github-client-a", "github-client-b"} {
		key := f.Web.CreateAPIKey(t, f.GroupID, label, []string{"ORG_ADMIN"})
		d.keys = append(d.keys, harness.Dial(t, f.Endpoint(harness.App).GRPC, key).Conn)
	}
	d.converge("initial")
	d.oauth("/auth/github/link/", true)
	d.oauth("/auth/github/app/link/", false)
	install := join(num(2, 101), num(3, 7))
	d.mutate("LinkGitHubAppInstallation", install, func() { d.m.installation = true; d.m.status = true })
	if strict {
		d.mutate("LinkGitHubRepo", str(2, repoURL), func() { d.m.repo = true })
		d.mutate("UnlinkGitHubAppInstallation", install, func() { d.m.installation = false; d.m.status = false })
		return
	}
	d.mutate("LinkGitHubAppInstallation", install, func() {}) // documented valid re-link
	for _, enabled := range []uint64{1, 1, 0, 1} {
		v := enabled
		d.mutate("UpdateGitHubAppInstallation", join(num(2, 7), str(3, "fixture-owner"), num(4, v)), func() { d.m.status = v == 1 })
	}
	d.mutate("LinkGitHubRepo", str(2, repoURL), func() { d.m.repo = true })
	for _, enabled := range []uint64{1, 1, 0, 1} {
		v := enabled
		d.mutate("UpdateGitHubRepoSettings", join(str(2, repoURL), num(3, v)), func() { d.m.defaults = v == 1 })
	}
	d.mutate("UnlinkGitHubRepo", str(2, repoURL), func() { d.m.repo = false; d.m.defaults = false })
	d.mutate("LinkGitHubRepo", str(2, repoURL), func() { d.m.repo = true })
	// Unlinking an installation intentionally preserves repository rows.
	d.mutate("UnlinkGitHubAppInstallation", install, func() { d.m.installation = false; d.m.status = false })
	d.mutate("LinkGitHubAppInstallation", install, func() { d.m.installation = true; d.m.status = true })
	d.mutate("UnlinkGitHubRepo", str(2, repoURL), func() { d.m.repo = false })
	d.mutate("UnlinkUserGitHubAccount", nil, func() { d.m.user = false })
	d.mutate("UnlinkUserGitHubAccount", nil, func() {})
	d.oauth("/auth/github/app/link/", false)
	d.mutate("UnlinkGitHubAccount", num(2, 1), func() { d.m.user = false })
	d.mutate("UnlinkGitHubAccount", nil, func() { d.m.group = false })
	d.mutate("UnlinkGitHubAppInstallation", install, func() { d.m.installation = false })
}

// Audit the code editor's repository reader and settings' install-path reader.
func (d *driver) auditReaders() error {
	extra := join(str(2, "fixture-owner"), str(3, "project"))
	req := map[string]any{"requestContext": d.f.Web.RequestContext(d.f.GroupID), "owner": "fixture-owner", "repo": "project"}
	if d.m.user {
		b, err := d.read("GetGithubRepo", extra)
		if err != nil {
			return err
		}
		if string(first(b, 2)) != "main" {
			return fmt.Errorf("GetGithubRepo default branch mismatch: %x", b)
		}
		j, err := d.jsonRPC("GetGithubRepo", req)
		if err != nil {
			return err
		}
		if j["defaultBranch"] != "main" {
			return fmt.Errorf("GetGithubRepo JSON branch mismatch: %v", j)
		}
	} else {
		_, err := d.grpc("GetGithubRepo", extra)
		code := "NotFound"
		if d.m.installation {
			code = "Unauthenticated"
		}
		if err == nil || !strings.Contains(err.Error(), "code = "+code) {
			return fmt.Errorf("GetGithubRepo denial want %s got %v", code, err)
		}
		for _, fn := range []func() error{
			func() error { _, e := d.http("GetGithubRepo", extra); return e },
			func() error { _, e := d.jsonRPC("GetGithubRepo", req); return e },
		} {
			e := fn()
			if e == nil || (!strings.Contains(e.Error(), "no linked GitHub account") && !strings.Contains(e.Error(), "github account link is required")) {
				return fmt.Errorf("GetGithubRepo HTTP denial: %v", e)
			}
		}
	}
	if d.m.user || d.m.installation {
		b, err := d.read("GetGitHubAppInstallPath", nil)
		if err != nil {
			return err
		}
		if string(first(b, 2)) != "/auth/github/app/link/" {
			return fmt.Errorf("install path mismatch: %x", b)
		}
		j, err := d.jsonRPC("GetGitHubAppInstallPath", map[string]any{"requestContext": d.f.Web.RequestContext(d.f.GroupID)})
		if err != nil {
			return err
		}
		if j["installPath"] != "/auth/github/app/link/" {
			return fmt.Errorf("JSON install path mismatch: %v", j)
		}
	} else {
		_, err := d.grpc("GetGitHubAppInstallPath", nil)
		if err == nil || !strings.Contains(err.Error(), "code = NotFound") {
			return fmt.Errorf("install path denial: %v", err)
		}
		_, err = d.http("GetGitHubAppInstallPath", nil)
		if err == nil || !strings.Contains(err.Error(), "no linked GitHub account") {
			return fmt.Errorf("HTTP install path denial: %v", err)
		}
		_, err = d.jsonRPC("GetGitHubAppInstallPath", map[string]any{"requestContext": d.f.Web.RequestContext(d.f.GroupID)})
		if err == nil || !strings.Contains(err.Error(), "no linked GitHub account") {
			return fmt.Errorf("JSON install path denial: %v", err)
		}
	}

	return nil
}
