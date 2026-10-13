package user_lists_test

import (
	"context"
	"fmt"
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

func TestLifecycle(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Config: map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_user_lists": true}}}})
	conn, err := grpc.NewClient(f.Endpoint(harness.App).GRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	adminKey := f.Web.CreateAPIKey(t, f.GroupID, "list-admin", []string{"ORG_ADMIN"})
	requestContext := field(1, str(2, f.GroupID))
	call := func(method string, payload []byte) ([]byte, error) {
		ctx, cancel := context.WithTimeout(harness.WithAPIKey(context.Background(), adminKey), 15*time.Second)
		defer cancel()
		req := &emptypb.Empty{}
		req.ProtoReflect().SetUnknown(join(requestContext, payload))
		res := &emptypb.Empty{}
		err := conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, req, res)
		return res.ProtoReflect().GetUnknown(), err
	}
	web := func(method string, req map[string]any) (map[string]any, error) {
		req["requestContext"] = f.Web.RequestContext(f.GroupID)
		return f.Web.RPC(method, req)
	}
	// Resolve the real fixture identity using both group-user surfaces.
	users, err := web("GetGroupUsers", map[string]any{"groupId": f.GroupID, "groupMembershipStatus": []string{"MEMBER"}})
	require.NoError(t, err)
	var identity member
	for _, u := range users["user"].([]any) {
		m := u.(map[string]any)
		if du, ok := m["user"].(map[string]any); ok {
			identity = jsonUser(du)
		}
	}
	require.NotEmpty(t, identity.ID)
	groupWire, err := call("GetGroupUsers", join(str(2, f.GroupID), field(3, []byte{1})))
	require.NoError(t, err)
	var resolved []member
	for _, u := range fields(t, groupWire, 2) {
		if du := one(t, u, 1); len(du) > 0 {
			resolved = append(resolved, wireUser(t, du))
		}
	}
	require.Contains(t, resolved, identity)
	model := map[string]list{}
	deleted := []string{}
	check := func() {
		t.Helper()
		b, err := call("GetUserLists", nil)
		require.NoError(t, err)
		got := map[string]list{}
		for _, v := range fields(t, b, 2) {
			l := wireList(t, v)
			require.NotContains(t, got, l.ID)
			got[l.ID] = l
		}
		require.Equal(t, model, got, "gRPC listing")
		j, err := web("GetUserLists", map[string]any{})
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
			b, err := call("GetUserList", str(2, id))
			require.NoError(t, err)
			require.Equal(t, want, wireList(t, one(t, b, 2)), "gRPC detail")
			j, err := web("GetUserList", map[string]any{"userListId": id})
			require.NoError(t, err)
			require.Equal(t, want, jsonList(j["userList"].(map[string]any)), "UI detail")
		}
		for _, id := range deleted {
			_, err := call("GetUserList", str(2, id))
			require.Equal(t, codes.NotFound, status.Code(err))
			_, err = web("GetUserList", map[string]any{"userListId": id})
			require.Error(t, err)
			require.True(t, strings.Contains(err.Error(), "not found"), "%v", err)
		}
	}
	check()
	create := func(name string) string {
		_, err := call("CreateUserList", str(2, name))
		require.NoError(t, err)
		b, err := call("GetUserLists", nil)
		require.NoError(t, err)
		id := ""
		for _, v := range fields(t, b, 2) {
			l := wireList(t, v)
			if _, exists := model[l.ID]; !exists {
				require.Equal(t, name, l.Name)
				require.Empty(t, l.Members)
				require.Empty(t, id)
				id = l.ID
			}
		}
		require.NotEmpty(t, id)
		model[id] = list{ID: id, Name: name}
		check()
		return id
	}
	a := create("alpha")
	b := create("sentinel")
	rename := func(id, name string) {
		_, err := call("UpdateUserList", field(2, join(str(1, id), str(2, name))))
		require.NoError(t, err)
		l := model[id]
		l.Name = name
		model[id] = l
		check()
	}
	rename(a, "renamed alpha")
	rename(a, "renamed alpha")
	membership := func(id string, action byte, want codes.Code) {
		update := join(field(1, str(1, identity.ID)), []byte{16, action})
		_, err := call("UpdateUserListMembership", join(str(2, id), field(3, update)))
		require.Equal(t, want, status.Code(err))
		if want == codes.OK {
			l := model[id]
			if action == 1 {
				l.Members = []member{identity}
			} else {
				l.Members = nil
			}
			model[id] = l
		}
		check()
	}
	membership(a, 1, codes.OK)
	rename(a, "populated alpha")
	membership(a, 1, codes.AlreadyExists)
	membership(b, 1, codes.OK)
	membership(a, 2, codes.OK)
	membership(a, 2, codes.NotFound)
	membership(a, 1, codes.OK)
	for _, id := range []string{a, b} {
		_, err := call("DeleteUserList", str(2, id))
		require.NoError(t, err)
		delete(model, id)
		deleted = append(deleted, id)
		check()
		_, err = call("DeleteUserList", str(2, id))
		require.Equal(t, codes.NotFound, status.Code(err))
		check()
	}
	c := create("renamed alpha")
	require.NotEqual(t, a, c)
	d := create("renamed alpha")
	require.NotEqual(t, c, d)
	check()
	t.Log(fmt.Sprintf("validated four mutations against gRPC and UI detail/listing; identity %s", identity.ID))
}
