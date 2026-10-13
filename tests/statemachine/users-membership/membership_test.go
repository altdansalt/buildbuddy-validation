package membership

import (
	"fmt"
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
	web            *harness.WebSession
	uid, home, key string
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
	if uid == "" {
		t.Fatalf("missing created user: %v", created)
	}
	w.UserID = map[string]any{"id": uid}
	got := mustHTTP(t, w, "GetUser", map[string]any{})
	if stringValue(object(object(got["displayUser"])["userId"])["id"]) != uid {
		t.Fatal("created user differs from GetUser")
	}
	home := stringValue(mustHTTP(t, w, "CreateGroup", map[string]any{"name": name + " home", "urlIdentifier": name + "-home", "userOwnedKeysEnabled": true})["id"])
	w.GroupID = home
	key := stringValue(object(mustHTTP(t, w, "CreateApiKey", map[string]any{"requestContext": w.RequestContext(home), "userId": uid, "label": "identity reader", "capability": []string{"ORG_ADMIN", "CACHE_WRITE", "CAS_WRITE"}})["apiKey"])["value"])
	if key == "" {
		t.Fatal("empty local user key")
	}
	return &identity{w, uid, home, key}
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
	deadline := time.Now().Add(25 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		last = observe(conn, admin, member, gid, m)
		if last == nil {
			return
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
		if err = compareUserHTTP(r, id.uid, gid, expected); err != nil {
			return err
		}
		g, err := rpc(conn, id.key, "GetUser", bytesField(2, requestContext(id.uid, gid)))
		if err != nil {
			return err
		}
		if err = compareUserGRPC(g, id.uid, gid, expected); err != nil {
			return err
		}
		// Public group metadata remains stable across every membership transition.
		r, err = id.web.RPC("GetGroup", map[string]any{"urlIdentifier": "membership-target"})
		if err != nil {
			return err
		}
		if r["id"] != gid || r["name"] != "Membership target" {
			return fmt.Errorf("HTTP group metadata: %v", r)
		}
		g, err = rpc(conn, id.key, "GetGroup", text(1, "membership-target"))
		if err != nil {
			return err
		}
		if string(first(g, 1)) != gid || string(first(g, 2)) != "Membership target" {
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
				continue
			}
			count++
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
		if count != wantCount {
			return fmt.Errorf("HTTP filtered roster count %d want %d", count, wantCount)
		}
		g, err := rpc(conn, admin.key, "GetGroupUsers", join(bytesField(1, requestContext(admin.uid, gid)), text(2, gid), bytesField(3, packed)))
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
				continue
			}
			count++
			if string(first(u, 2)) != fmt.Sprint(m.status) || string(first(u, 3)) != fmt.Sprint(m.role) {
				return fmt.Errorf("gRPC roster role/status differs from %+v: %v", m, u)
			}
		}
		if count != wantCount {
			return fmt.Errorf("gRPC filtered roster count %d want %d", count, wantCount)
		}
	}
	return nil
}
func TestMembershipStateMachine(t *testing.T) {
	p := startIssuer(t)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, SkipAuthBootstrap: true,
		DockerArgs: map[harness.Component][]string{harness.App: {"--add-host=host.docker.internal:host-gateway"}},
		Config:     map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_self_auth": false, "disable_refresh_token": true, "oauth_providers": []any{harness.Config{"issuer_url": p.url, "client_id": "membership-client", "client_secret": "fixture-secret"}}}, "app": harness.Config{"create_group_per_user": false, "user_owned_keys_enabled": true}}}})
	admin := createIdentity(t, f, p, "admin")
	member := createIdentity(t, f, p, "member")
	gid := stringValue(mustHTTP(t, admin.web, "CreateGroup", map[string]any{"name": "Membership target", "urlIdentifier": "membership-target", "userOwnedKeysEnabled": true})["id"])
	// The admin key must belong to the target organization for organization reads.
	admin.key = stringValue(object(mustHTTP(t, admin.web, "CreateApiKey", map[string]any{"requestContext": admin.web.RequestContext(gid), "userId": admin.uid, "label": "target admin", "capability": []string{"ORG_ADMIN", "CACHE_WRITE", "CAS_WRITE"}})["apiKey"])["value"])
	conn := f.Clients(harness.App).Conn
	state := model{}
	check(t, conn, admin, member, gid, state)
	steps := []struct {
		name         string
		action, role int
		join         bool
		repeat       bool
		want         model
	}{
		{name: "request", join: true, want: model{2, 1}},
		{name: "repeat-request", join: true, repeat: true, want: model{2, 1}},
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
				_, err = rpc(conn, admin.key, "UpdateGroupUsers", join(bytesField(1, requestContext(admin.uid, gid)), text(2, gid), bytesField(3, update)))
			}
			if s.repeat {
				if status.Code(err) != codes.AlreadyExists {
					t.Fatalf("expected AlreadyExists for repeat, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			state = s.want
			check(t, conn, admin, member, gid, state)
		})
	}
	// Re-login the removed user: a fresh session must see the same state.
	member.web = login(t, f, p, "member")
	member.web.UserID = map[string]any{"id": member.uid}
	check(t, conn, admin, member, gid, state)
	t.Logf("images: app=%s", f.Endpoint(harness.App).Image)
}

var _ = strings.Contains
