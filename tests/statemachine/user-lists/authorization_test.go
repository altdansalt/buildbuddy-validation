package user_lists_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Short TTLs bound this conformance test; these are explicitly non-default.
// The owner loses its direct membership, so only the list grants key access.
func TestIndirectAuthorization(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy},
		Config: map[harness.Component]harness.Config{
			harness.App:        {"auth": harness.Config{"enable_user_lists": true, "api_key_group_cache_ttl": "1s"}, "app": harness.Config{"user_owned_keys_enabled": true}},
			harness.CacheProxy: {"auth": harness.Config{"remote": harness.Config{"jwt_cache_ttl": "1s"}}},
		}})
	web := func(method string, body map[string]any) map[string]any {
		body["requestContext"] = f.Web.RequestContext(f.GroupID)
		res, err := f.Web.RPC(method, body)
		require.NoError(t, err)
		return res
	}
	users := web("GetGroupUsers", map[string]any{"groupId": f.GroupID, "groupMembershipStatus": []string{"MEMBER"}})
	identity := jsonUser(users["user"].([]any)[0].(map[string]any)["user"].(map[string]any))
	web("UpdateGroup", map[string]any{"id": f.GroupID, "name": "indirect authorization", "userOwnedKeysEnabled": true})
	owner := web("CreateApiKey", map[string]any{"userId": identity.ID, "label": "indirect-owner", "capability": []string{"CAS_WRITE", "CACHE_WRITE"}})["apiKey"].(map[string]any)["value"].(string)
	admin := f.Web.CreateAPIKey(t, f.GroupID, "independent-org-admin", []string{"ORG_ADMIN"})
	conn, err := grpc.NewClient(f.Endpoint(harness.App).GRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	mutate := func(method string, payload []byte) []byte {
		ctx, cancel := context.WithTimeout(harness.WithAPIKey(context.Background(), admin), 10*time.Second)
		defer cancel()
		req, res := &emptypb.Empty{}, &emptypb.Empty{}
		req.ProtoReflect().SetUnknown(join(field(1, str(2, f.GroupID)), payload))
		require.NoError(t, conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, req, res))
		return res.ProtoReflect().GetUnknown()
	}
	mutate("CreateUserList", str(2, "sole access"))
	lists := mutate("GetUserLists", nil)
	id := wireList(t, fields(t, lists, 2)[0]).ID
	mutate("UpdateGroupUsers", join(str(2, f.GroupID), field(3, join(str(4, id), []byte{16, 1, 24, 1}))))
	membership := func(action byte) {
		mutate("UpdateUserListMembership", join(str(2, id), field(3, join(field(1, str(1, identity.ID)), []byte{16, action}))))
	}
	membership(1)
	mutate("UpdateGroupUsers", join(str(2, f.GroupID), field(3, join(field(1, str(1, identity.ID)), []byte{16, 2}))))
	app := harness.Dial(t, f.Endpoint(harness.App).GRPC, owner)
	proxy := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, owner)
	check := func(want bool) {
		t.Helper()
		start := time.Now()
		probe := func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Distinct payloads force real writes rather than cached reads.
			_, ae := app.Upload(ctx, "", []byte("app authorization "+time.Now().String()))
			_, pe := proxy.Upload(ctx, "", []byte("proxy authorization "+time.Now().String()))
			res, err := f.Web.RPC("GetUser", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
			require.NoError(t, err)
			found := false
			groups, _ := res["userGroup"].([]any)
			for _, g := range groups {
				if g.(map[string]any)["groupId"] == f.GroupID {
					found = true
				}
			}
			return (ae == nil) == want && (pe == nil) == want && found == want
		}
		deadline := time.Now().Add(20 * time.Second)
		for !probe() {
			require.True(t, time.Now().Before(deadline), "app/proxy/GetUser did not converge")
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("authorization=%v converged in %s (1s app/proxy TTL configuration)", want, time.Since(start))
		for i := 0; i < 3; i++ {
			time.Sleep(400 * time.Millisecond)
			require.True(t, probe(), "converged authorization changed")
		}
	}
	check(true)
	mutate("UpdateUserList", field(2, join(str(1, id), str(2, "renamed sole access"))))
	check(true)
	membership(2)
	check(false)
	membership(1)
	check(true)
	mutate("DeleteUserList", str(2, id))
	check(false)
}
