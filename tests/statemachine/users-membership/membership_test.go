package membership

import (
	"context"
	"fmt"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type identity struct {
	ownedDomain               string
	writeSequence             int
	seed                      *repb.Digest
	fixtureHTTP               string
	web                       *harness.WebSession
	uid, home, key, manageKey string
	profile                   map[string]any
	targetKey, targetID       string
	storedCaps                int
	cacheClients              []*harness.Clients
}

func mustHTTP(t *testing.T, w *harness.WebSession, method string, req map[string]any) map[string]any {
	t.Helper()
	r, err := w.RPC(method, req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return r
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func stringValue(v any) string    { s, _ := v.(string); return s }
func createIdentity(t *testing.T, f *harness.Fixture, p *issuer, name string) *identity {
	w := login(t, f, p, name)
	created := mustHTTP(t, w, "CreateUser", map[string]any{})
	uid := stringValue(object(object(created["displayUser"])["userId"])["id"])
	if object(created["displayUser"])["email"] != name+"@membership.test" {
		t.Fatalf("created profile differs from OIDC identity: %v", created)
	}
	if uid == "" {
		t.Fatalf("missing created user: %v", created)
	}
	w.UserID = map[string]any{"id": uid}
	_, repeatErr := w.RPC("CreateUser", map[string]any{})
	if repeatErr == nil || !strings.Contains(repeatErr.Error(), "code = FailedPrecondition") {
		t.Fatalf("repeat CreateUser should preserve identity and reject recreation: %v", repeatErr)
	}
	got := mustHTTP(t, w, "GetUser", map[string]any{})
	if stringValue(object(object(got["displayUser"])["userId"])["id"]) != uid ||
		object(got["displayUser"])["email"] != name+"@membership.test" {
		t.Fatal("created user differs from GetUser")
	}
	home := stringValue(mustHTTP(t, w, "CreateGroup", map[string]any{"name": name + " home", "urlIdentifier": name + "-home", "userOwnedKeysEnabled": true})["id"])
	w.GroupID = home
	key := stringValue(object(mustHTTP(t, w, "CreateUserApiKey", map[string]any{"requestContext": w.RequestContext(home), "userId": uid, "label": "identity reader", "capability": []string{"CACHE_WRITE", "CAS_WRITE"}})["apiKey"])["value"])
	if key == "" {
		t.Fatal("empty local user key")
	}
	return &identity{web: w, uid: uid, home: home, key: key, profile: object(created["displayUser"])}
}

// The reference model tracks just the target organization. The home group
// remains present so removed identities can still authenticate normally.
type model struct{ status, role int }

var roleName = map[int]string{1: "DEVELOPER_ROLE", 2: "ADMIN_ROLE", 4: "WRITER_ROLE", 8: "READER_ROLE"}
var capabilities = map[int][]string{1: {"CAS_WRITE"}, 2: {"CACHE_WRITE", "CAS_WRITE", "ORG_ADMIN"}, 4: {"CACHE_WRITE", "CAS_WRITE"}, 8: {}}

func contains(a []any, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func compareUserHTTP(r map[string]any, uid, gid string, m model) error {
	if stringValue(object(object(r["displayUser"])["userId"])["id"]) != uid {
		return fmt.Errorf("identity mismatch: %v", r)
	}
	count := 0
	for _, v := range array(r["userGroup"]) {
		g := object(v)
		if g["id"] != gid {
			continue
		}
		count++
		got := map[string]bool{}
		for _, c := range array(g["capabilities"]) {
			got[fmt.Sprint(c)] = true
		}
		want := map[string]bool{}
		for _, c := range capabilities[m.role] {
			want[c] = true
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("HTTP capabilities %v want %v", got, want)
		}
	}
	wantCount := 0
	if m.status == 1 {
		wantCount = 1
	}
	if count != wantCount {
		return fmt.Errorf("HTTP membership count %d want %d", count, wantCount)
	}
	if m.status == 1 {
		if object(r["selectedGroup"])["groupId"] != gid {
			return fmt.Errorf("selected group: %v", r["selectedGroup"])
		}
		if contains(array(r["allowedRpc"]), "UpdateGroupUsers") != (m.role == 2) {
			return fmt.Errorf("role not reflected in allowedRpc: %v", r["allowedRpc"])
		}
	} else if object(r["selectedGroup"])["groupId"] == gid {
		return fmt.Errorf("nonmember selected target group")
	}
	return nil
}
func compareUserGRPC(r fields, uid, gid string, m model) error {
	display, err := decode(first(r, 1))
	if err != nil {
		return err
	}
	id, err := decode(first(display, 1))
	if err != nil {
		return err
	}
	if string(first(id, 1)) != uid {
		return fmt.Errorf("gRPC identity mismatch")
	}
	count := 0
	for _, b := range r[2] {
		g, err := decode(b)
		if err != nil {
			return err
		}
		if string(first(g, 1)) != gid {
			continue
		}
		count++
		// capabilities are packed enum values, field 20.
		got := map[byte]bool{}
		for _, raw := range g[20] {
			for _, v := range raw {
				got[v] = true
			}
		}
		want := map[byte]bool{}
		switch m.role {
		case 1:
			want[4] = true
		case 2:
			want[1] = true
			want[4] = true
			want[8] = true
		case 4:
			want[1] = true
			want[4] = true
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("gRPC capabilities %v want %v", got, want)
		}
	}
	want := 0
	if m.status == 1 {
		want = 1
	}
	if count != want {
		return fmt.Errorf("gRPC membership count %d want %d", count, want)
	}
	return nil
}
func check(t *testing.T, conn *grpc.ClientConn, admin, member *identity, gid string, m model) {
	t.Helper()
	began := time.Now()
	deadline := began.Add(25 * time.Second)
	var stableSince time.Time
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		last = observe(conn, admin, member, gid, m)
		if last == nil && member.targetKey != "" {
			last = observeTargetKey(conn, admin, member, gid, m)
		}
		if last == nil {
			if stableSince.IsZero() {
				stableSince = time.Now()
				t.Logf("state %+v converged after %s", m, time.Since(began))
			}
			if time.Since(stableSince) >= time.Second {
				return
			}
		} else {
			stableSince = time.Time{}
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %+v did not converge: %v", m, last)
		}
		<-ticker.C
	}
}
func observe(conn *grpc.ClientConn, admin, member *identity, gid string, m model) error {
	for _, id := range []*identity{admin, member} {
		expected := m
		if id == admin {
			expected = model{1, 2}
		}
		r, err := id.web.RPC("GetUser", map[string]any{"requestContext": id.web.RequestContext(gid)})
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(object(r["displayUser"]), id.profile) {
			return fmt.Errorf("profile changed: %v want %v", r["displayUser"], id.profile)
		}
		if len(array(r["userGroup"])) != 1+func() int {
			if expected.status == 1 {
				return 1
			}
			return 0
		}() {
			return fmt.Errorf("unexpected organizations: %v", r["userGroup"])
		}
		if object(r["selectedGroup"])["access"] != "ALLOWED" || r["selectedGroupId"] != object(r["selectedGroup"])["groupId"] {
			return fmt.Errorf("selected access/deprecated ID: %v", r)
		}
		hpRPC := []string{}
		for _, v := range array(r["allowedRpc"]) {
			hpRPC = append(hpRPC, fmt.Sprint(v))
		}
		if err := comparePermissions(hpRPC, func() int {
			if expected.status != 1 {
				return 1
			}
			if expected.role == 2 {
				return 1
			}
			return 0
		}()); err != nil {
			return err
		}
		if err = compareUserHTTP(r, id.uid, gid, expected); err != nil {
			return err
		}
		if id == member {
			// A real authenticated organization read must enforce the current
			// role, independently of the UI's advertised allowedRpc list.
			_, readErr := id.web.RPC("GetGroupUsers", map[string]any{
				"requestContext": id.web.RequestContext(gid), "groupId": gid,
				"groupMembershipStatus": []string{"MEMBER", "REQUESTED"},
			})
			if expected.status == 1 && expected.role == 2 {
				if readErr != nil {
					return fmt.Errorf("admin member read: %w", readErr)
				}
			} else if readErr == nil || !strings.Contains(readErr.Error(), "code = PermissionDenied") {
				return fmt.Errorf("nonadmin organization read should be denied: %v", readErr)
			}
		}
		g, err := rpc(conn, id.key, "GetUser", bytesField(2, requestContext(id.uid, id.home)))
		if err != nil {
			return fmt.Errorf("gRPC GetUser uid=%s: %w", id.uid, err)
		}
		gpRPC := []string{}
		for _, v := range g[4] {
			gpRPC = append(gpRPC, string(v))
		}
		if err := comparePermissions(gpRPC, 0); err != nil {
			return fmt.Errorf("gRPC permissions: %w", err)
		}
		sg, err := decode(first(g, 9))
		if err != nil {
			return err
		}
		if string(first(sg, 1)) != id.home || string(first(sg, 2)) != "1" || string(first(g, 3)) != id.home {
			return fmt.Errorf("gRPC selected home/access differs: %v", g)
		}
		if err = compareUserGRPC(g, id.uid, gid, expected); err != nil {
			return err
		}
		display, err := decode(first(g, 1))
		if err != nil {
			return err
		}
		name, err := decode(first(display, 2))
		if err != nil {
			return err
		}
		hp := object(r["displayUser"])
		hn := object(hp["name"])
		for field, key := range map[int]string{1: "full", 2: "first", 3: "last"} {
			if string(first(name, field)) != stringValue(hn[key]) {
				return fmt.Errorf("gRPC profile name %s differs", key)
			}
		}
		if string(first(display, 3)) != stringValue(hp["profileImageUrl"]) || string(first(display, 6)) != stringValue(hp["username"]) || string(first(display, 5)) != "4" {
			return fmt.Errorf("gRPC profile differs: %v", display)
		}
		if len(g[2]) != len(array(r["userGroup"])) {
			return fmt.Errorf("gRPC unrelated membership count differs")
		}
		if string(first(display, 4)) != stringValue(object(r["displayUser"])["email"]) {
			return fmt.Errorf("identity email differs across HTTP and gRPC")
		}
		// Public group metadata remains stable across every membership transition.
		r, err = id.web.RPC("GetGroup", map[string]any{"urlIdentifier": "membership-target"})
		if err != nil {
			return err
		}
		if r["id"] != gid || r["name"] != "Membership target" || stringValue(r["ownedDomain"]) != admin.ownedDomain {
			return fmt.Errorf("HTTP group metadata: %v", r)
		}
		g, err = rpc(conn, id.key, "GetGroup", text(1, "membership-target"))
		if err != nil {
			return err
		}
		if string(first(g, 1)) != gid || string(first(g, 2)) != "Membership target" || string(first(g, 3)) != admin.ownedDomain {
			return fmt.Errorf("gRPC group metadata: %v", g)
		}
	}
	// Both status filters independently and their union must match the model.
	for _, statuses := range [][]int{{1}, {2}, {1, 2}} {
		enum := []string{}
		var packed []byte
		wanted := false
		for _, s := range statuses {
			packed = append(packed, byte(s))
			if s == 1 {
				enum = append(enum, "MEMBER")
			} else {
				enum = append(enum, "REQUESTED")
			}
			if s == m.status {
				wanted = true
			}
		}
		r, err := admin.web.RPC("GetGroupUsers", map[string]any{"requestContext": admin.web.RequestContext(gid), "groupId": gid, "groupMembershipStatus": enum})
		if err != nil {
			return err
		}
		count := 0
		for _, v := range array(r["user"]) {
			u := object(v)
			if object(object(u["user"])["userId"])["id"] != member.uid {
				if object(object(u["user"])["userId"])["id"] != admin.uid || u["role"] != "ADMIN_ROLE" || u["groupMembershipStatus"] != "MEMBER" || !reflect.DeepEqual(object(u["user"]), admin.profile) {
					return fmt.Errorf("unexpected HTTP roster row: %v", u)
				}
				continue
			}
			count++
			if object(u["user"])["email"] != "member@membership.test" {
				return fmt.Errorf("HTTP roster identity profile differs: %v", u)
			}
			expectedStatus := "MEMBER"
			if m.status == 2 {
				expectedStatus = "REQUESTED"
			}
			if u["role"] != roleName[m.role] || u["groupMembershipStatus"] != expectedStatus {
				return fmt.Errorf("HTTP roster: %v model %+v", u, m)
			}
		}
		wantCount := 0
		if wanted {
			wantCount = 1
		}
		total := wantCount
		for _, s := range statuses {
			if s == 1 {
				total++
			}
		}
		if len(array(r["user"])) != total {
			return fmt.Errorf("HTTP full roster size %d want %d", len(array(r["user"])), total)
		}
		if count != wantCount {
			return fmt.Errorf("HTTP filtered roster count %d want %d", count, wantCount)
		}
		g, err := rpc(conn, admin.manageKey, "GetGroupUsers", join(bytesField(1, requestContext(admin.uid, gid)), text(2, gid), bytesField(3, packed)))
		if err != nil {
			return err
		}
		count = 0
		for _, b := range g[2] {
			u, err := decode(b)
			if err != nil {
				return err
			}
			d, err := decode(first(u, 1))
			if err != nil {
				return err
			}
			id, err := decode(first(d, 1))
			if err != nil {
				return err
			}
			if string(first(id, 1)) != member.uid {
				if string(first(id, 1)) != admin.uid || string(first(u, 2)) != "1" || string(first(u, 3)) != "2" {
					return fmt.Errorf("unexpected gRPC roster row: %v", u)
				}
				continue
			}
			count++
			if string(first(d, 4)) != "member@membership.test" {
				return fmt.Errorf("gRPC roster identity profile differs")
			}
			if string(first(u, 2)) != fmt.Sprint(m.status) || string(first(u, 3)) != fmt.Sprint(m.role) {
				return fmt.Errorf("gRPC roster role/status differs from %+v: %v", m, u)
			}
		}
		if len(g[2]) != total {
			return fmt.Errorf("gRPC full roster size %d want %d", len(g[2]), total)
		}
		if count != wantCount {
			return fmt.Errorf("gRPC filtered roster count %d want %d", count, wantCount)
		}
	}
	return nil
}
func TestMembershipStateMachine(t *testing.T) {
	cli := downloadCLI(t)
	p := startIssuer(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, SkipAuthBootstrap: true,
		Args:   map[harness.Component][]string{harness.App: {"--auth.api_key_group_cache_ttl=0"}, harness.CacheProxy: {"--auth.remote.jwt_cache_ttl=2s"}},
		Config: map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_self_auth": false, "disable_refresh_token": true, "oauth_providers": []any{harness.Config{"issuer_url": p.url, "client_id": "membership-client", "client_secret": "fixture-secret"}}}, "app": harness.Config{"create_group_per_user": false, "user_owned_keys_enabled": true}}}})
	admin := createIdentity(t, f, p, "admin")
	member := createIdentity(t, f, p, "member")
	gid := stringValue(mustHTTP(t, admin.web, "CreateGroup", map[string]any{"requestContext": admin.web.RequestContext(admin.home), "name": "Membership target", "urlIdentifier": "membership-target", "userOwnedKeysEnabled": true})["id"])
	// The admin key must belong to the target organization for organization reads.
	admin.manageKey = stringValue(object(mustHTTP(t, admin.web, "CreateApiKey", map[string]any{"requestContext": admin.web.RequestContext(gid), "userId": admin.uid, "label": "target admin", "capability": []string{"ORG_ADMIN", "CACHE_WRITE", "CAS_WRITE"}})["apiKey"])["value"])
	conn := f.Clients(harness.App).Conn
	seedCtx, seedCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer seedCancel()
	seedClient := harness.Dial(t, f.Endpoint(harness.App).GRPC, admin.manageKey)
	var seedErr error
	member.seed, seedErr = seedClient.Upload(seedCtx, "", []byte("membership surface seed"))
	if seedErr != nil {
		t.Fatal(seedErr)
	}
	_, seedErr = seedClient.AC.UpdateActionResult(seedCtx, &repb.UpdateActionResultRequest{ActionDigest: member.seed, ActionResult: &repb.ActionResult{ExitCode: 17}})
	if seedErr != nil {
		t.Fatal(seedErr)
	}
	member.fixtureHTTP = f.Endpoint(harness.App).HTTP
	state := model{}
	check(t, conn, admin, member, gid, state)
	checkCLI(t, cli, f.Endpoint(harness.App).GRPC, member, state)
	checkCacheWrites(t, member, seedClient, state)
	steps := []struct {
		name         string
		action, role int
		join         bool
		repeat       bool
		want         model
	}{
		{name: "request", join: true, want: model{2, 1}},
		{name: "repeat-request", join: true, repeat: true, want: model{2, 1}},
		{name: "pending-reader-role", role: 8, want: model{2, 8}},
		{name: "cancel-request", action: 2, want: model{}},
		{name: "repeat-remove", action: 2, want: model{}},
		{name: "request-again", join: true, want: model{2, 1}},
		{name: "approve", action: 1, role: 1, want: model{1, 1}},
		{name: "repeat-add", action: 1, repeat: true, want: model{1, 1}},
		{name: "join-existing", join: true, repeat: true, want: model{1, 1}},
		{name: "writer", role: 4, want: model{1, 4}},
		{name: "reader", role: 8, want: model{1, 8}},
		{name: "admin", role: 2, want: model{1, 2}},
		{name: "repeat-role", role: 2, want: model{1, 2}},
		{name: "developer", role: 1, want: model{1, 1}},
		{name: "remove-member", action: 2, want: model{}},
		{name: "direct-add", action: 1, role: 4, want: model{1, 4}},
		{name: "final-remove", action: 2, want: model{}},
	}
	outer := t
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			var err error
			if s.join {
				_, err = rpc(conn, member.key, "JoinGroup", join(text(1, gid), bytesField(2, requestContext(member.uid, member.home))))
			} else {
				update := bytesField(1, text(1, member.uid))
				if s.action != 0 {
					update = append(update, number(2, uint64(s.action))...)
				}
				if s.role != 0 {
					update = append(update, number(3, uint64(s.role))...)
				}
				updates := bytesField(3, update)
				if s.name == "writer" {
					// Batch updates must preserve the independently modeled admin.
					updates = append(updates, bytesField(3, join(bytesField(1, text(1, admin.uid)), number(3, 2)))...)
				}
				_, err = rpc(conn, admin.manageKey, "UpdateGroupUsers", join(bytesField(1, requestContext(admin.uid, gid)), text(2, gid), updates))
			}
			if s.repeat {
				if status.Code(err) != codes.AlreadyExists {
					t.Fatalf("expected AlreadyExists for repeat, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if s.role != 0 && member.targetKey != "" {
				prev := state.role
				if state.status == 0 {
					prev = 1 // a fresh ADD creates the membership as developer first
				}
				if s.role != prev {
					member.storedCaps = storedAfterRoleChange(member.storedCaps, s.role)
				}
			}
			state = s.want
			if s.name == "approve" {
				member.storedCaps = 1 | 4
				k := object(mustHTTP(t, admin.web, "CreateUserApiKey", map[string]any{"requestContext": admin.web.RequestContext(gid), "userId": member.uid, "label": "sequence key", "capability": []string{"CACHE_WRITE", "CAS_WRITE"}})["apiKey"])
				member.targetKey = stringValue(k["value"])
				member.targetID = stringValue(k["id"])
				member.cacheClients = []*harness.Clients{harness.Dial(outer, f.Endpoint(harness.App).GRPC, member.targetKey), harness.Dial(outer, f.Endpoint(harness.CacheProxy).GRPC, member.targetKey)}
			}
			check(t, conn, admin, member, gid, state)
			checkCLI(t, cli, f.Endpoint(harness.App).GRPC, member, state)
			checkCacheWrites(t, member, seedClient, state)
		})
	}
	// Configure an owned-domain join policy using the ordinary UI setting.
	// JoinGroup then establishes membership directly instead of requesting it.
	mustHTTP(t, admin.web, "UpdateGroup", map[string]any{
		"requestContext": admin.web.RequestContext(gid), "id": gid,
		"name": "Membership target", "urlIdentifier": "membership-target",
		"autoPopulateFromOwnedDomain": true, "userOwnedKeysEnabled": true,
	})
	admin.ownedDomain = "membership.test"
	check(t, conn, admin, member, gid, state)
	for i := 0; i < 2; i++ {
		_, err := rpc(conn, member.key, "JoinGroup", join(text(1, gid), bytesField(2, requestContext(member.uid, member.home))))
		if err != nil {
			t.Fatalf("owned-domain join %d: %v", i, err)
		}
		state = model{1, 1}
		check(t, conn, admin, member, gid, state)
		checkCLI(t, cli, f.Endpoint(harness.App).GRPC, member, state)
		checkCacheWrites(t, member, seedClient, state)
		if i == 0 {
			_, err = rpc(conn, admin.manageKey, "UpdateGroupUsers", join(bytesField(1, requestContext(admin.uid, gid)), text(2, gid), bytesField(3, join(bytesField(1, text(1, member.uid)), number(2, 2)))))
			if err != nil {
				t.Fatal(err)
			}
			state = model{}
			check(t, conn, admin, member, gid, state)
			checkCLI(t, cli, f.Endpoint(harness.App).GRPC, member, state)
			checkCacheWrites(t, member, seedClient, state)
		}
	}
	_, err := rpc(conn, admin.manageKey, "UpdateGroupUsers", join(bytesField(1, requestContext(admin.uid, gid)), text(2, gid), bytesField(3, join(bytesField(1, text(1, member.uid)), number(2, 2)))))
	if err != nil {
		t.Fatal(err)
	}
	state = model{}
	check(t, conn, admin, member, gid, state)
	checkCLI(t, cli, f.Endpoint(harness.App).GRPC, member, state)
	checkCacheWrites(t, member, seedClient, state)
	// Re-login the removed user: a fresh session must see the same state.
	member.web = login(t, f, p, "member")
	member.web.UserID = map[string]any{"id": member.uid}
	check(t, conn, admin, member, gid, state)
	checkCLI(t, cli, f.Endpoint(harness.App).GRPC, member, state)
	checkCacheWrites(t, member, seedClient, state)
	t.Logf("images: app=%s", f.Endpoint(harness.App).Image)
}
