package user_lists_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Wire fields follow released v2.314.0 proto/{user_list,context,grp,user_id}.proto.
// Empty preserves unknown fields, avoiding a dependency on BuildBuddy source.
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func str(n protowire.Number, s string) []byte { return field(n, []byte(s)) }
func join(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}
func fields(t *testing.T, b []byte, n protowire.Number) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		k, typ, l := protowire.ConsumeTag(b)
		require.Greater(t, l, 0)
		b = b[l:]
		size := protowire.ConsumeFieldValue(k, typ, b)
		require.GreaterOrEqual(t, size, 0)
		if k == n && typ == protowire.BytesType {
			v, l := protowire.ConsumeBytes(b)
			require.Greater(t, l, 0)
			out = append(out, v)
		}
		b = b[size:]
	}
	return out
}
func one(t *testing.T, b []byte, n protowire.Number) []byte {
	a := fields(t, b, n)
	if len(a) == 0 {
		return nil
	}
	require.Len(t, a, 1)
	return a[0]
}

type member struct{ ID, Email string }
type list struct {
	ID, Name string
	Members  []member
}

func wireUser(t *testing.T, b []byte) member {
	return member{string(one(t, one(t, b, 1), 1)), string(one(t, b, 4))}
}
func wireList(t *testing.T, b []byte) list {
	l := list{ID: string(one(t, b, 1)), Name: string(one(t, b, 2))}
	for _, u := range fields(t, b, 3) {
		l.Members = append(l.Members, wireUser(t, u))
	}
	normalize(&l)
	return l
}
func normalize(l *list) {
	sort.Slice(l.Members, func(i, j int) bool { return l.Members[i].ID < l.Members[j].ID })
}
func jsonUser(m map[string]any) member {
	id, _ := m["userId"].(map[string]any)
	s, _ := id["id"].(string)
	e, _ := m["email"].(string)
	return member{s, e}
}
func jsonList(m map[string]any) list {
	id, _ := m["userListId"].(string)
	name, _ := m["name"].(string)
	l := list{ID: id, Name: name}
	us, _ := m["user"].([]any)
	for _, u := range us {
		l.Members = append(l.Members, jsonUser(u.(map[string]any)))
	}
	normalize(&l)
	return l
}

// rig reads one org's user-list state through every surface the product
// exposes: two independent admin API keys over gRPC, the cookie-authenticated
// UI protolet API, GetGroupUsers (list attachments) on both transports and,
// when configured, the SCIM v2 Groups endpoints.
type rig struct {
	t        *testing.T
	f        *harness.Fixture
	conn     *grpc.ClientConn
	keys     []string // independent ORG_ADMIN keys, all in the same org
	identity member
	scimKey  string
	scimBase string
	attached bool // lists are attached to the org so GetGroupUsers shows them
}

func newRig(t *testing.T, f *harness.Fixture) *rig {
	conn, err := grpc.NewClient(f.Endpoint(harness.App).GRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	r := &rig{t: t, f: f, conn: conn, attached: true}
	r.keys = []string{f.Web.CreateAPIKey(t, f.GroupID, "list-admin-a", []string{"ORG_ADMIN"}), f.Web.CreateAPIKey(t, f.GroupID, "list-admin-b", []string{"ORG_ADMIN"})}
	// Resolve the real fixture identity using both group-user surfaces.
	users, err := r.web("GetGroupUsers", map[string]any{"groupId": f.GroupID, "groupMembershipStatus": []string{"MEMBER"}})
	require.NoError(t, err)
	for _, u := range users["user"].([]any) {
		if du, ok := u.(map[string]any)["user"].(map[string]any); ok {
			r.identity = jsonUser(du)
		}
	}
	require.NotEmpty(t, r.identity.ID)
	groupWire, err := r.call(0, "GetGroupUsers", join(str(2, f.GroupID), field(3, []byte{1})))
	require.NoError(t, err)
	var resolved []member
	for _, u := range fields(t, groupWire, 2) {
		if du := one(t, u, 1); len(du) > 0 {
			resolved = append(resolved, wireUser(t, du))
		}
	}
	require.Contains(t, resolved, r.identity)
	return r
}

func (r *rig) call(key int, method string, payload []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(harness.WithAPIKey(context.Background(), r.keys[key]), 15*time.Second)
	defer cancel()
	req, res := &emptypb.Empty{}, &emptypb.Empty{}
	req.ProtoReflect().SetUnknown(join(field(1, str(2, r.f.GroupID)), payload))
	err := r.conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, req, res)
	return res.ProtoReflect().GetUnknown(), err
}

func (r *rig) web(method string, req map[string]any) (map[string]any, error) {
	req["requestContext"] = r.f.Web.RequestContext(r.f.GroupID)
	return r.f.Web.RPC(method, req)
}

func (r *rig) must(key int, method string, payload []byte) []byte {
	r.t.Helper()
	b, err := r.call(key, method, payload)
	require.NoError(r.t, err, method)
	return b
}

// create issues CreateUserList and returns the new ID found by diffing the listing.
func (r *rig) create(key int, name string, model map[string]list) string {
	r.t.Helper()
	r.must(key, "CreateUserList", str(2, name))
	id := ""
	for _, v := range fields(r.t, r.must(1-key, "GetUserLists", nil), 2) {
		l := wireList(r.t, v)
		if _, exists := model[l.ID]; !exists {
			require.Equal(r.t, name, l.Name)
			require.Empty(r.t, l.Members)
			require.Empty(r.t, id)
			id = l.ID
		}
	}
	require.NotEmpty(r.t, id)
	return id
}

func (r *rig) attach(id string) {
	r.t.Helper()
	r.must(0, "UpdateGroupUsers", join(str(2, r.f.GroupID), field(3, join(str(4, id), []byte{16, 1, 24, 1}))))
}

func (r *rig) scim(method, path string, body any, out any) int {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(r.t, err)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, r.scimBase+"/scim/v2/Groups"+path, rd)
	require.NoError(r.t, err)
	req.Header.Set("Authorization", "Bearer "+r.scimKey)
	req.Header.Set("Content-Type", "application/scim+json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(r.t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && out != nil {
		require.NoError(r.t, json.Unmarshal(b, out), string(b))
	}
	return resp.StatusCode
}

// check compares the model to every surface; deleted IDs must be absent everywhere.
func (r *rig) check(model map[string]list, deleted []string) {
	t := r.t
	t.Helper()
	wantNames := map[string]string{}
	for id, l := range model {
		wantNames[id] = l.Name
	}
	for k := range r.keys {
		got := map[string]list{}
		for _, v := range fields(t, r.must(k, "GetUserLists", nil), 2) {
			l := wireList(t, v)
			require.NotContains(t, got, l.ID)
			got[l.ID] = l
		}
		require.Equal(t, model, got, "gRPC listing, key %d", k)
		for id, want := range model {
			require.Equal(t, want, wireList(t, one(t, r.must(k, "GetUserList", str(2, id)), 2)), "gRPC detail, key %d", k)
		}
		groupBytes := r.must(k, "GetGroupUsers", join(str(2, r.f.GroupID), field(3, []byte{1})))
		grpcNames := map[string]string{}
		for _, gu := range fields(t, groupBytes, 2) {
			if ul := one(t, gu, 4); len(ul) > 0 {
				l := wireList(t, ul)
				require.NotContains(t, grpcNames, l.ID)
				grpcNames[l.ID] = l.Name
				require.Empty(t, l.Members, "GetGroupUsers returns list summaries")
			}
		}
		if r.attached {
			require.Equal(t, wantNames, grpcNames, "gRPC GetGroupUsers list summaries, key %d", k)
		}
	}
	j, err := r.web("GetUserLists", map[string]any{})
	require.NoError(t, err)
	httpLists := map[string]list{}
	vs, _ := j["userList"].([]any)
	for _, v := range vs {
		l := jsonList(v.(map[string]any))
		require.NotContains(t, httpLists, l.ID)
		httpLists[l.ID] = l
	}
	require.Equal(t, model, httpLists, "UI listing")
	for id, want := range model {
		j, err := r.web("GetUserList", map[string]any{"userListId": id})
		require.NoError(t, err)
		require.Equal(t, want, jsonList(j["userList"].(map[string]any)), "UI detail")
	}
	groupJSON, err := r.web("GetGroupUsers", map[string]any{"groupId": r.f.GroupID, "groupMembershipStatus": []string{"MEMBER"}})
	require.NoError(t, err)
	httpNames := map[string]string{}
	for _, v := range groupJSON["user"].([]any) {
		gu := v.(map[string]any)
		if ul, ok := gu["userList"].(map[string]any); ok {
			l := jsonList(ul)
			require.NotContains(t, httpNames, l.ID)
			httpNames[l.ID] = l.Name
			require.Equal(t, "DEVELOPER_ROLE", gu["role"])
			require.Equal(t, "MEMBER", gu["groupMembershipStatus"])
		}
	}
	if r.attached {
		require.Equal(t, wantNames, httpNames, "UI GetGroupUsers list summaries")
	}
	for _, id := range deleted {
		for k := range r.keys {
			_, err := r.call(k, "GetUserList", str(2, id))
			require.Equal(t, codes.NotFound, status.Code(err))
		}
		_, err = r.web("GetUserList", map[string]any{"userListId": id})
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "not found"), "%v", err)
	}
	if r.scimBase != "" {
		var res struct {
			Total     int         `json:"totalResults"`
			Resources []scimGroup `json:"Resources"`
		}
		require.Equal(t, 200, r.scim("GET", "", nil, &res))
		sg := map[string]list{}
		for _, g := range res.Resources {
			require.NotContains(t, sg, g.ID)
			sg[g.ID] = g.list()
		}
		require.Equal(t, model, sg, "SCIM listing")
		require.Equal(t, len(model), res.Total)
		for id, want := range model {
			var g scimGroup
			require.Equal(t, 200, r.scim("GET", "/"+id, nil, &g))
			require.Equal(t, want, g.list(), "SCIM detail")
			var filtered struct {
				Resources []scimGroup `json:"Resources"`
			}
			require.Equal(t, 200, r.scim("GET", "?filter="+url.QueryEscape(fmt.Sprintf("displayName eq %q", want.Name)), nil, &filtered))
			found := 0
			for _, fg := range filtered.Resources {
				require.Equal(t, want.Name, fg.Name)
				if fg.ID == id {
					found++
					require.Equal(t, want, fg.list(), "SCIM filtered")
				}
			}
			require.Equal(t, 1, found, "SCIM filter finds %s", id)
		}
		for _, id := range deleted {
			require.Equal(t, 404, r.scim("GET", "/"+id, nil, nil))
		}
	}
}

func TestLifecycle(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_user_lists": true}}}})
	r := newRig(t, f)
	model := map[string]list{}
	deleted := []string{}
	step := 0
	check := func() {
		t.Helper()
		step++
		r.check(model, deleted)
	}
	check()
	// Mutations alternate between the two independent API keys; reads use both.
	create := func(key int, name string) string {
		id := r.create(key, name, model)
		r.attach(id)
		model[id] = list{ID: id, Name: name}
		check()
		return id
	}
	a := create(0, "alpha")
	b := create(1, "sentinel")
	rename := func(key int, id, name string) {
		r.must(key, "UpdateUserList", field(2, join(str(1, id), str(2, name))))
		l := model[id]
		l.Name = name
		model[id] = l
		check()
	}
	rename(1, a, "renamed alpha")
	rename(0, a, "renamed alpha")
	rename(1, a, "second rename")
	rename(0, a, "renamed alpha")
	membership := func(key int, id string, action byte, want codes.Code) {
		update := join(field(1, str(1, r.identity.ID)), []byte{16, action})
		_, err := r.call(key, "UpdateUserListMembership", join(str(2, id), field(3, update)))
		require.Equal(t, want, status.Code(err))
		if want == codes.OK {
			l := model[id]
			if action == 1 {
				l.Members = []member{r.identity}
			} else {
				l.Members = nil
			}
			model[id] = l
		}
		check()
	}
	membership(0, a, 1, codes.OK)
	rename(1, a, "populated alpha")
	membership(1, a, 1, codes.AlreadyExists)
	membership(1, b, 1, codes.OK)
	membership(0, a, 2, codes.OK)
	membership(1, a, 2, codes.NotFound)
	membership(0, a, 1, codes.OK)
	for i, id := range []string{a, b} {
		r.must(i, "DeleteUserList", str(2, id))
		delete(model, id)
		deleted = append(deleted, id)
		check()
		_, err := r.call(1-i, "DeleteUserList", str(2, id))
		require.Equal(t, codes.NotFound, status.Code(err))
		check()
	}
	c := create(0, "renamed alpha")
	require.NotEqual(t, a, c)
	d := create(1, "renamed alpha")
	require.NotEqual(t, c, d)
	membership(1, c, 1, codes.OK)
	membership(0, d, 1, codes.OK)
	rename(0, c, "gamma")
	// Converged state must be stable.
	for i := 0; i < 3; i++ {
		time.Sleep(time.Second)
		check()
	}
	t.Logf("validated %d full cross-surface checks; identity %s; synchronous SQL state, no polling needed", step, r.identity.ID)
}

// With lists unattached to the org, GetUserLists must be unaffected and
// GetGroupUsers must not show them.
func TestUnattachedLists(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_user_lists": true}}}})
	r := newRig(t, f)
	r.attached = false
	model := map[string]list{}
	id := r.create(0, "unattached", model)
	model[id] = list{ID: id, Name: "unattached"}
	r.check(model, nil)
	for _, v := range fields(t, r.must(0, "GetGroupUsers", join(str(2, f.GroupID), field(3, []byte{1}))), 2) {
		require.Empty(t, one(t, v, 4), "unattached list leaked into GetGroupUsers")
	}
	r.attach(id)
	r.attached = true
	r.check(model, nil)
	r.must(1, "DeleteUserList", str(2, id))
	delete(model, id)
	r.check(model, []string{id})
}
