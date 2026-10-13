package groups_test

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/jhump/protoreflect/desc/protoparse"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Released v2.314.0 wire schemas, copied locally to avoid a BuildBuddy source dependency.
//
//go:embed proto/*.proto
var schemas embed.FS

type rpcClient struct {
	conn     *grpc.ClientConn
	messages map[string]protoreflect.MessageDescriptor
}

func descriptors(t *testing.T) map[string]protoreflect.MessageDescriptor {
	t.Helper()
	c := protoparse.Parser{Accessor: func(p string) (io.ReadCloser, error) { f, e := schemas.Open(p); return f, e }}
	files, err := c.ParseFiles("proto/grp.proto", "proto/user.proto", "proto/bazel_config.proto", "proto/api_key.proto", "proto/surfaces.proto")
	require.NoError(t, err)
	result := map[string]protoreflect.MessageDescriptor{}
	for _, f := range files {
		for i := 0; i < f.UnwrapFile().Messages().Len(); i++ {
			m := f.UnwrapFile().Messages().Get(i)
			result[string(m.Name())] = m
		}
	}
	result["GetUserApiKeysRequest"] = result["GetApiKeysRequest"]
	result["GetUserApiKeysResponse"] = result["GetApiKeysResponse"]
	result["GetUserApiKeyRequest"] = result["GetApiKeyRequest"]
	result["GetUserApiKeyResponse"] = result["GetApiKeyResponse"]
	return result
}
func (c rpcClient) call(method string, req map[string]any) (map[string]any, error) {
	in := dynamicpb.NewMessage(c.messages[method+"Request"])
	out := dynamicpb.NewMessage(c.messages[method+"Response"])
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err = protojson.Unmarshal(b, in); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = c.conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, in, out); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	b, err = (protojson.MarshalOptions{}).Marshal(out)
	if err != nil {
		return nil, err
	}
	var r map[string]any
	err = json.Unmarshal(b, &r)
	return r, err
}
func must(t *testing.T, c rpcClient, method string, req map[string]any) map[string]any {
	t.Helper()
	r, e := c.call(method, req)
	require.NoError(t, e, method)
	return r
}
func equivalent(a, b any) bool {
	var normalize func(any)
	normalize = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if k == "capability" || k == "capabilities" || k == "allowedUserApiKeyCapabilities" {
					if list, ok := val.([]any); ok {
						sort.Slice(list, func(i, j int) bool { return fmt.Sprint(list[i]) < fmt.Sprint(list[j]) })
					}
				}
				normalize(val)
			}
		case []any:
			for _, val := range x {
				normalize(val)
			}
		}
	}
	normalize(a)
	normalize(b)
	return reflect.DeepEqual(a, b)
}
func num(v any) float64 {
	if v == nil {
		return 0
	}
	if n, ok := v.(float64); ok {
		return n
	}
	return map[string]float64{"ENABLED": 1, "ADMINS_ONLY": 2, "DISABLED": 3, "FREE_TIER_GROUP_STATUS": 1, "ENTERPRISE_GROUP_STATUS": 2, "ENTERPRISE_TRIAL_GROUP_STATUS": 3, "BLOCKED_GROUP_STATUS": 4, "USAGE_BASED_GROUP_STATUS": 5, "OPEN_SOURCE_GROUP_STATUS": 6}[v.(string)]
}
func value(m map[string]any, k string) any {
	if v, ok := m[k]; ok {
		return v
	}
	return false
}

func TestOrganizationTransitions(t *testing.T) {
	ds := descriptors(t)
	indexTarget, indexCalls := indexFixture(t, ds)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, DockerArgs: map[harness.Component][]string{harness.App: {"--add-host", "host.docker.internal:host-gateway"}}, Config: map[harness.Component]harness.Config{harness.App: {"remote_execution": harness.Config{"enable_user_owned_executors": true, "require_executor_authorization": true}, "app": harness.Config{"no_default_user_group": false, "create_group_per_user": false, "user_owned_keys_enabled": true, "codesearch_backend": indexTarget}, "auth": harness.Config{"admin_group_id": "GR0000000000000000000", "api_key_group_cache_ttl": "1s"}}, harness.CacheProxy: {"auth": harness.Config{"remote": harness.Config{"jwt_cache_ttl": "1s"}}}}})
	cli, cliDir, cliEnv := releasedCLI(t)
	clients := map[string]rpcClient{}
	client := func(key string) rpcClient {
		if c, ok := clients[key]; ok {
			return c
		}
		c := rpcClient{harness.Dial(t, f.Endpoint(harness.App).GRPC, key).Conn, ds}
		clients[key] = c
		return c
	}
	web := func(method string, req map[string]any) map[string]any {
		t.Helper()
		r, e := f.Web.RPC(method, req)
		require.NoError(t, e, method)
		return r
	}
	rc := func(id string) map[string]any { return map[string]any{"requestContext": f.Web.RequestContext(id)} }
	// Enable a user-owned key in the bootstrap admin organization for user-identity gRPC calls.
	web("UpdateGroup", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "name": "Server admins", "urlIdentifier": "server-admins", "developerOrgCreationEnabled": true, "userOwnedKeysEnabled": true})
	web("SetGroupStatus", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "status": 2})
	uk := web("CreateUserApiKey", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "label": "identity", "capability": []string{"CACHE_WRITE"}})["apiKey"].(map[string]any)["value"].(string)
	identity := client(uk)
	adminKey := f.Web.CreateAPIKey(t, f.GroupID, "server-admin", []string{"ORG_ADMIN", "CACHE_WRITE"})
	type model struct {
		id, name, slug, domain, url           string
		fields                                map[string]any
		status                                float64
		key                                   string
		userKey                               string
		developerKey, invocationID, userKeyID string
		public                                bool
		directory, action                     *repb.Digest
		data                                  []byte
	}
	models := []*model{}
	cacheClients := map[string]*harness.Clients{}
	cacheClient := func(component harness.Component, key string) *harness.Clients {
		k := string(component) + key
		if c, ok := cacheClients[k]; ok {
			return c
		}
		c := harness.Dial(t, f.Endpoint(component).GRPC, key)
		cacheClients[k] = c
		return c
	}
	checks := func() error {
		for _, m := range models {
			req := rc(m.id)
			// User-owned key belongs to admin group: GetUser still returns all user's memberships.
			userReq := rc(f.GroupID)
			a, e := identity.call("GetUser", userReq)
			if e != nil {
				return e
			}
			b, e := f.Web.RPC("GetUser", req)
			if e != nil {
				return e
			}
			if b["selectedGroup"].(map[string]any)["access"] != "ALLOWED" {
				return fmt.Errorf("HTTP selected organization access denied")
			}
			if a["selectedGroup"].(map[string]any)["access"] != "ALLOWED" {
				return fmt.Errorf("gRPC selected organization access denied")
			}
			if b["selectedGroup"].(map[string]any)["groupId"] != m.id {
				return fmt.Errorf("HTTP selected organization differs")
			}
			if a["selectedGroup"].(map[string]any)["groupId"] != f.GroupID {
				return fmt.Errorf("gRPC selected organization differs")
			}
			for _, surface := range []map[string]any{a, b} {
				if len(surface["userGroup"].([]any)) != len(models)+1 {
					return fmt.Errorf("membership count differs: %v", surface["userGroup"])
				}
				var g map[string]any
				for _, item := range surface["userGroup"].([]any) {
					v := item.(map[string]any)
					if v["id"] == m.id {
						g = v
					}
				}
				if g == nil {
					return fmt.Errorf("missing membership %s", m.id)
				}
				if g["name"] != m.name || g["urlIdentifier"] != m.slug || g["url"] != m.url {
					return fmt.Errorf("identity mismatch: %v", g)
				}
				domain, _ := g["ownedDomain"].(string)
				if domain != m.domain {
					return fmt.Errorf("domain mismatch %q != %q", domain, m.domain)
				}
				for k, v := range m.fields {
					actual := value(g, k)
					if k == "suggestionPreference" {
						actual = num(g[k])
					}
					if !equivalent(actual, v) {
						return fmt.Errorf("%s: %v != %v", k, actual, v)
					}
				}
				if num(g["status"]) != m.status {
					return fmt.Errorf("status mismatch: %v != %v", g["status"], m.status)
				}
			}

			// Membership and API-key settings surfaces.
			membersReq := rc(m.id)
			membersReq["groupId"] = m.id
			membersReq["groupMembershipStatus"] = []string{"MEMBER"}
			members, e := f.Web.RPC("GetGroupUsers", membersReq)
			if e != nil {
				return fmt.Errorf("HTTP GetGroupUsers: %w", e)
			}
			rows, ok := members["user"].([]any)
			if !ok || len(rows) != 1 {
				return fmt.Errorf("expected one member: %v", members)
			}
			member := rows[0].(map[string]any)
			display := member["user"].(map[string]any)
			if !equivalent(display["userId"], a["displayUser"].(map[string]any)["userId"]) || member["role"] != "ADMIN_ROLE" || member["groupMembershipStatus"] != "MEMBER" {
				return fmt.Errorf("member identity/role/status: %v", member)
			}
			if m.status != 4 {
				gm, e := client(m.key).call("GetGroupUsers", membersReq)
				if e != nil {
					return fmt.Errorf("gRPC GetGroupUsers: %w", e)
				}
				if !equivalent(gm["user"], members["user"]) {
					return fmt.Errorf("membership transport divergence: %v / %v", gm, members)
				}
			}
			keys, e := f.Web.RPC("GetUserApiKeys", rc(m.id))
			if m.fields["userOwnedKeysEnabled"] == true {
				if e != nil {
					return fmt.Errorf("HTTP GetUserApiKeys: %w", e)
				}
				list, ok := keys["apiKey"].([]any)
				if !ok || len(list) != 1 || list[0].(map[string]any)["label"] != "personal" || list[0].(map[string]any)["id"] != m.userKeyID {
					return fmt.Errorf("personal key inventory: %v", keys)
				}
				if _, present := list[0].(map[string]any)["value"]; present {
					return fmt.Errorf("personal inventory leaked key value")
				}
				if m.status != 4 {
					gk, e := client(m.userKey).call("GetUserApiKeys", rc(m.id))
					if e != nil {
						return fmt.Errorf("gRPC GetUserApiKeys: %w", e)
					}
					if !equivalent(gk["apiKey"], keys["apiKey"]) {
						return fmt.Errorf("key-list transport divergence: %v / %v", gk, keys)
					}
				}
			} else if e == nil || !strings.Contains(e.Error(), "code = PermissionDenied") || !strings.Contains(e.Error(), "user-owned keys are not enabled for this group") {
				return fmt.Errorf("disabled user-key inventory denial mismatch: %v / %v", keys, e)
			}

			// The executor settings UI marks organization-owned workers as default.
			nodes, e := f.Web.RPC("GetExecutionNodes", rc(m.id))
			if e != nil {
				return e
			}
			if nodes["userOwnedExecutorsSupported"] != true {
				return fmt.Errorf("owned executor support missing")
			}
			entries, ok := nodes["executor"].([]any)
			if !ok || len(entries) != 1 {
				return fmt.Errorf("owned executor inventory: %v", nodes)
			}
			row := entries[0].(map[string]any)
			if value(row, "isDefault") != m.fields["useGroupOwnedExecutors"] {
				return fmt.Errorf("executor default policy differs: %v", row)
			}
			if m.status != 4 {
				other, e := client(m.key).call("GetExecutionNodes", rc(m.id))
				if e != nil {
					return e
				}
				list, ok := other["executor"].([]any)
				if !ok || len(list) != 1 {
					return fmt.Errorf("gRPC owned executor inventory: %v", other)
				}
				r := list[0].(map[string]any)
				if value(r, "isDefault") != m.fields["useGroupOwnedExecutors"] || r["node"].(map[string]any)["executorId"] != row["node"].(map[string]any)["executorId"] {
					return fmt.Errorf("executor transport policy divergence")
				}
			}
			worker := row["node"].(map[string]any)["executorHostId"].(string)
			if e := checkOwnedExecution(cacheClient(harness.App, m.key), m.action, m.id, worker, m.fields["useGroupOwnedExecutors"] == true, m.status == 4); e != nil {
				return e
			}
			ownerReq := map[string]any{"invocationId": m.invocationID}
			owner, e := client(adminKey).call("GetInvocationOwner", ownerReq)
			if e != nil {
				return e
			}
			httpOwner, e := f.Web.RPC("GetInvocationOwner", ownerReq)
			if e != nil {
				return e
			}
			if !equivalent(owner, httpOwner) || owner["groupId"] != m.id || owner["groupUrl"] != m.url {
				return fmt.Errorf("invocation owner metadata model mismatch: %v", owner)
			}
			// A valid namespace drop reaches the local backend exactly when
			// the released app's organization code-search gate permits it.
			before := len(indexCalls())
			indexReq := rc(m.id)
			indexReq["namespace"] = m.id
			indexReq["replacementStrategy"] = 3
			_, e = f.Web.RPC("Index", indexReq)
			enabled := m.fields["codeSearchEnabled"] == true
			if enabled {
				if e != nil {
					return e
				}
			} else if e == nil || !strings.Contains(e.Error(), "code = FailedPrecondition") || !strings.Contains(e.Error(), "codesearch is not enabled") {
				return fmt.Errorf("web code-search gate: %v", e)
			}
			expectedCalls := 0
			if enabled {
				expectedCalls++
			}
			if m.status != 4 {
				_, e = client(m.key).call("Index", indexReq)
				if enabled {
					if e != nil {
						return e
					}
					expectedCalls++
				} else if status.Code(e) != codes.FailedPrecondition {
					return fmt.Errorf("gRPC code-search gate: %v", e)
				}
			}
			calls := indexCalls()
			if len(calls)-before != expectedCalls {
				return fmt.Errorf("code-search forwarding count mismatch")
			}
			for _, namespace := range calls[before:] {
				if namespace != m.id {
					return fmt.Errorf("code-search namespace mismatch: %s", namespace)
				}
			}
			// Sharing is enforced by the real ACL mutation, rather than just a UI flag.
			shareReq := rc(m.id)
			shareReq["invocationId"] = m.invocationID
			shareReq["acl"] = map[string]any{"groupId": m.id, "ownerPermissions": map[string]any{"read": true, "write": true}, "groupPermissions": map[string]any{"read": true, "write": true}, "othersPermissions": map[string]any{"read": true}}
			_, shareErr := f.Web.RPC("UpdateInvocation", shareReq)
			if m.fields["sharingEnabled"] == true {
				if shareErr != nil {
					return shareErr
				}
				m.public = true
			} else if shareErr == nil || !strings.Contains(shareErr.Error(), "Your organization does not allow this action") {
				return fmt.Errorf("sharing gate: %v", shareErr)
			}
			if m.status != 4 {
				_, e := client(m.key).call("UpdateInvocation", shareReq)
				if m.fields["sharingEnabled"] == true {
					if e != nil {
						return e
					}
				} else if status.Code(e) != codes.PermissionDenied {
					return fmt.Errorf("gRPC sharing gate: %v", e)
				}
				// Valid cache snapshot identity: invalidation of absent snapshots is supported.
				snapshotReq := rc(m.id)
				snapshotReq["snapshotKey"] = map[string]any{"snapshotId": m.invocationID, "instanceName": "groups"}
				_, e = client(m.developerKey).call("InvalidateSnapshot", snapshotReq)
				if m.fields["restrictCleanWorkflowRunsToAdmins"] == true {
					if status.Code(e) != codes.PermissionDenied {
						return fmt.Errorf("workflow clean-run gate: %v", e)
					}
				} else if e != nil {
					return e
				}
				_, e = f.Web.RPC("InvalidateSnapshot", snapshotReq)
				if e != nil {
					return fmt.Errorf("web admin clean-run gate: %w", e)
				}
			}
			invocationReq := rc(m.id)
			invocationReq["lookup"] = map[string]any{"invocationId": m.invocationID}
			invocation, e := f.Web.RPC("GetInvocation", invocationReq)
			if e != nil {
				return e
			}
			rowsInv, ok := invocation["invocation"].([]any)
			if !ok || len(rowsInv) != 1 {
				return fmt.Errorf("invocation inventory mismatch: %v", invocation)
			}
			inv := rowsInv[0].(map[string]any)
			if inv["invocationId"] != m.invocationID {
				return fmt.Errorf("invocation identity mismatch")
			}
			acl := inv["acl"].(map[string]any)
			if acl["groupId"] != m.id || value(acl["othersPermissions"].(map[string]any), "read") != m.public {
				return fmt.Errorf("sharing ACL model mismatch: %v", acl)
			}
			if m.status != 4 {
				other, e := client(m.key).call("GetInvocation", invocationReq)
				if e != nil {
					return e
				}
				if !equivalent(other["invocation"].([]any)[0].(map[string]any)["acl"], acl) {
					return fmt.Errorf("ACL transport mismatch")
				}
			}
			anonymous, e := client("").call("GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": m.invocationID}})
			if m.public {
				if e != nil {
					return e
				}
				if anonymous["invocation"].([]any)[0].(map[string]any)["invocationId"] != m.invocationID {
					return fmt.Errorf("public invocation differs")
				}
			} else if status.Code(e) != codes.PermissionDenied && !(status.Code(e) == codes.Unauthenticated && strings.Contains(e.Error(), "User not found")) {
				return fmt.Errorf("private invocation read: %v", e)
			}
			logReq := rc(m.id)
			logReq["invocationId"] = m.invocationID
			logReq["type"] = 1
			logs, e := f.Web.RPC("GetEventLogChunk", logReq)
			if e != nil {
				return e
			}
			label := inv["user"].(string)
			data, e := base64.StdEncoding.DecodeString(logs["buffer"].(string))
			if e != nil {
				return e
			}
			if string(data) != label+"\n" {
				return fmt.Errorf("log projection mismatch: %q", data)
			}
			if m.status != 4 {
				other, e := client(m.key).call("GetEventLogChunk", logReq)
				if e != nil {
					return e
				}
				if !equivalent(other, logs) {
					return fmt.Errorf("log transport mismatch")
				}
			}
			if e := checkDownloads(f.Endpoint(harness.App).HTTP, m.invocationID, fmt.Sprint(inv["attempt"]), label, m.public, f.Web.Client, f.Web.URL); e != nil {
				return e
			}
			// A blocked group must reject each authenticated serving RPC,
			// not merely one representative cache or configuration request.
			if m.status == 4 {
				keyReq := rc(m.id)
				keyReq["apiKeyId"] = m.userKeyID
				snapshotReq := rc(m.id)
				snapshotReq["snapshotKey"] = map[string]any{"snapshotId": m.invocationID, "instanceName": "groups"}
				for _, probe := range []struct {
					method, key string
					request     map[string]any
				}{
					{"GetUser", m.userKey, rc(m.id)},
					{"GetGroupUsers", m.key, membersReq},
					{"GetApiKeys", m.key, rc(m.id)},
					{"GetUserApiKeys", m.userKey, rc(m.id)},
					{"GetApiKey", m.key, keyReq},
					{"GetUserApiKey", m.userKey, keyReq},
					{"GetExecutionNodes", m.key, rc(m.id)},
					{"GetInvocation", m.key, invocationReq},
					{"GetEventLogChunk", m.key, logReq},
					{"Index", m.key, indexReq},
					{"UpdateInvocation", m.key, shareReq},
					{"InvalidateSnapshot", m.developerKey, snapshotReq},
				} {
					_, e := client(probe.key).call(probe.method, probe.request)
					if status.Code(e) != codes.Unavailable {
						return fmt.Errorf("blocked %s authorization: %v", probe.method, e)
					}
				}
			} else if m.fields["userOwnedKeysEnabled"] == true {
				selected, e := client(m.userKey).call("GetUser", rc(m.id))
				if e != nil {
					return e
				}
				selection := selected["selectedGroup"].(map[string]any)
				if selection["groupId"] != m.id || selection["access"] != "ALLOWED" || !equivalent(selected["userGroup"], b["userGroup"]) {
					return fmt.Errorf("personal-key selected group / memberships mismatch")
				}
			}
			// Settings inventories omit secrets; value retrieval is a separate UI RPC.
			orgKeys, e := f.Web.RPC("GetApiKeys", rc(m.id))
			if e != nil {
				return e
			}
			inventory, ok := orgKeys["apiKey"].([]any)
			if !ok || len(inventory) != 2 {
				return fmt.Errorf("org inventory: %v", orgKeys)
			}
			labels := map[string]bool{}
			for _, row := range inventory {
				labels[row.(map[string]any)["label"].(string)] = true
			}
			if !labels["cache"] || !labels["Default"] {
				return fmt.Errorf("org key label: %v", inventory)
			}
			if m.status != 4 {
				other, e := client(m.key).call("GetApiKeys", rc(m.id))
				if e != nil {
					return e
				}
				if !equivalent(other["apiKey"], inventory) {
					return fmt.Errorf("org key inventory transports differ: %v / %v", other, orgKeys)
				}
			}
			for _, spec := range []struct {
				method   string
				list     []any
				expected string
				enabled  bool
			}{
				{"GetApiKey", inventory, m.key, true},
				{"GetUserApiKey", []any{map[string]any{"id": m.userKeyID, "label": "personal"}}, m.userKey, true},
			} {
				if !spec.enabled {
					continue
				}
				for _, row := range spec.list {
					k := row.(map[string]any)
					if _, present := k["value"]; present {
						return fmt.Errorf("inventory leaked value: %s", spec.method)
					}
					r := rc(m.id)
					r["apiKeyId"] = k["id"]
					if spec.method == "GetUserApiKey" && m.fields["userOwnedKeysEnabled"] != true {
						_, e := f.Web.RPC(spec.method, r)
						if e == nil || !strings.Contains(e.Error(), "code = NotFound") || !strings.Contains(e.Error(), "The requested API key was not found") {
							return fmt.Errorf("disabled user key value visibility: %v", e)
						}
						if m.status != 4 {
							_, e = client(m.key).call(spec.method, r)
							if status.Code(e) != codes.NotFound {
								return fmt.Errorf("gRPC disabled user key value visibility: %v", e)
							}
							_, e = client(m.userKey).call(spec.method, r)
							if status.Code(e) != codes.Unauthenticated && status.Code(e) != codes.PermissionDenied {
								return fmt.Errorf("disabled user key value authorization: %v", e)
							}
						}
						continue
					}
					got, e := f.Web.RPC(spec.method, r)
					if e != nil {
						return e
					}
					v := got["apiKey"].(map[string]any)
					expected := spec.expected
					if k["label"] == "Default" {
						expected = m.developerKey
					}
					if v["value"] != expected || v["id"] != k["id"] || v["label"] != k["label"] {
						return fmt.Errorf("%s value/model mismatch", spec.method)
					}
					if m.status != 4 {
						credential := m.key
						if spec.method == "GetUserApiKey" && m.fields["userOwnedKeysEnabled"] == true {
							credential = m.userKey
						}

						other, e := client(credential).call(spec.method, r)
						if e != nil {
							return e
						}
						if !equivalent(other["apiKey"], v) {
							return fmt.Errorf("%s value transports differ", spec.method)
						}
					}
				}
			}
			for _, lookup := range []map[string]any{{"urlIdentifier": m.slug}, {"groupId": m.id, "requestContext": f.Web.RequestContext(f.GroupID)}} {
				gr, e := client(adminKey).call("GetGroup", lookup)
				if e != nil {
					return e
				}
				hr, e := f.Web.RPC("GetGroup", lookup)
				if e != nil {
					return e
				}
				if !equivalent(gr, hr) {
					return fmt.Errorf("GetGroup surfaces differ: %v / %v", gr, hr)
				}
				if gr["id"] != m.id || gr["name"] != m.name || gr["url"] != m.url {
					return fmt.Errorf("GetGroup model mismatch: %v", gr)
				}
				d, _ := gr["ownedDomain"].(string)
				if d != m.domain {
					return fmt.Errorf("public domain mismatch")
				}
			}
			cfgReq := rc(m.id)
			cfgReq["host"] = "app"
			cfgReq["protocol"] = "http:"
			hc, e := f.Web.RPC("GetBazelConfig", cfgReq)
			if e != nil {
				return e
			}
			personal := false
			if list, ok := hc["credential"].([]any); ok {
				for _, v := range list {
					k := v.(map[string]any)["apiKey"].(map[string]any)
					if k["label"] == "personal" {
						personal = true
					}
				}
			}
			if personal != (m.fields["userOwnedKeysEnabled"] == true) {
				return fmt.Errorf("personal credential projection mismatch")
			}
			gc, e := client(m.userKey).call("GetBazelConfig", cfgReq)
			allowed := m.fields["userOwnedKeysEnabled"] == true && m.status != 4
			if allowed {
				if e != nil {
					return e
				}
				if !equivalent(gc["configOption"], hc["configOption"]) {
					return fmt.Errorf("Bazel configuration options differ")
				}
				// User-owned credentials deliberately lack ORG_ADMIN; hidden org keys are web-admin only.
				visible := map[string]any{}
				if list, ok := hc["credential"].([]any); ok {
					for _, v := range list {
						k := v.(map[string]any)["apiKey"].(map[string]any)
						visible[k["id"].(string)] = v
					}
				}
				if list, ok := gc["credential"].([]any); ok {
					for _, v := range list {
						k := v.(map[string]any)["apiKey"].(map[string]any)
						if !equivalent(visible[k["id"].(string)], v) {
							return fmt.Errorf("credential metadata differs")
						}
						delete(visible, k["id"].(string))
					}
				}
				for _, v := range visible {
					k := v.(map[string]any)["apiKey"].(map[string]any)
					if k["visibleToDevelopers"] == true {
						return fmt.Errorf("visible credential missing from gRPC config")
					}
				}
			} else {
				code := status.Code(e)
				if m.fields["userOwnedKeysEnabled"] == true {
					if code != codes.Unavailable {
						return fmt.Errorf("blocked config code: %v", e)
					}
				} else if code != codes.Unauthenticated && code != codes.PermissionDenied {
					return fmt.Errorf("disabled config code: %v", e)
				}
			}
			expectedOptions := []any{
				map[string]any{"body": "build --bes_results_url=http://app:8080/invocation/", "optionLifecycle": "build", "flagName": "bes_results_url", "flagValue": "http://app:8080/invocation/"},
				map[string]any{"body": "build --bes_backend=grpc://app:1985", "optionLifecycle": "build", "flagName": "bes_backend", "flagValue": "grpc://app:1985"},
				map[string]any{"body": "common --remote_cache=grpc://app:1985", "optionLifecycle": "common", "flagName": "remote_cache", "flagValue": "grpc://app:1985"},
				map[string]any{"body": "common --remote_executor=grpc://app:1985", "optionLifecycle": "common", "flagName": "remote_executor", "flagValue": "grpc://app:1985"},
			}
			if !equivalent(expectedOptions, hc["configOption"]) {
				return fmt.Errorf("configuration model mismatch: %v", hc["configOption"])
			}
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				for _, key := range []string{m.userKey, m.key} {
					expectedAllowed := key == m.key && m.status != 4 || key == m.userKey && allowed
					c := cacheClient(component, key)
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					missing, ce := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{harness.Digest(m.data)}})
					ac, ae := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: harness.Digest(m.data)})
					batch, be := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{harness.Digest(m.data)}})
					stream, te := c.CAS.GetTree(ctx, &repb.GetTreeRequest{RootDigest: m.directory})
					var tree *repb.GetTreeResponse
					if te == nil {
						tree, te = stream.Recv()
						if te == nil {
							_, end := stream.Recv()
							if end != io.EOF {
								te = fmt.Errorf("GetTree did not terminate: %v", end)
							}
						}
					}
					cancel()
					if expectedAllowed {
						if ce != nil {
							return ce
						}
						if ae != nil {
							return ae
						}
						if be != nil {
							return be
						}
						if te != nil {
							return te
						}
						if len(batch.GetResponses()) != 1 || batch.Responses[0].GetStatus().GetCode() != 0 || string(batch.Responses[0].GetData()) != string(m.data) {
							return fmt.Errorf("%s CAS batch model mismatch", component)
						}
						if len(tree.GetDirectories()) != 1 || len(tree.Directories[0].GetFiles()) != 1 || tree.Directories[0].Files[0].GetDigest().GetHash() != harness.Digest(m.data).Hash {
							return fmt.Errorf("%s streamed tree mismatch", component)
						}
						if len(missing.GetMissingBlobDigests()) != 0 || string(ac.GetStdoutRaw()) != string(m.data) {
							return fmt.Errorf("%s CAS/AC model mismatch", component)
						}
					} else {
						for _, err := range []error{ce, ae, be, te} {
							code := status.Code(err)
							if m.status == 4 && (key == m.key || m.fields["userOwnedKeysEnabled"] == true) {
								if code != codes.Unavailable && !(component == harness.CacheProxy && code == codes.Unauthenticated && strings.Contains(err.Error(), "code = Unavailable")) {
									return fmt.Errorf("%s blocked CAS/AC: %v", component, err)
								}
							} else if code != codes.Unauthenticated && code != codes.PermissionDenied {
								return fmt.Errorf("%s disabled CAS/AC: %v", component, err)
							}
						}
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				data, err := cacheClient(component, m.userKey).Read(ctx, "", harness.Digest(m.data))
				cancel()
				if allowed {
					if err != nil {
						return err
					}
					if !equivalent(data, m.data) {
						return fmt.Errorf("cache content mismatch")
					}
				} else {
					code := status.Code(err)
					if m.fields["userOwnedKeysEnabled"] == true {
						if code != codes.Unavailable {
							return fmt.Errorf("%s blocked cache code: %v", component, err)
						}
					} else if code != codes.Unauthenticated && code != codes.PermissionDenied {
						return fmt.Errorf("%s disabled cache code: %v", component, err)
					}
				}
			}
		}
		return nil
	}
	convergence := []string{}
	t.Cleanup(func() {
		b, e := json.MarshalIndent(map[string]any{"appImage": f.Endpoint(harness.App).Image, "proxyImage": f.Endpoint(harness.CacheProxy).Image, "authCacheTTL": "1s", "stableSweeps": 3, "stableInterval": "400ms", "observedConvergence": convergence}, "", "  ")
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(f.OutputDir, "group-convergence.json"), b, 0600))
	})
	verify := func() {
		t.Helper()
		started := time.Now()
		deadline := started.Add(20 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var err error
		lastError := ""
		for {
			err = checks()
			if err != nil && err.Error() != lastError {
				lastError = err.Error()
				t.Logf("awaiting convergence at %s: %v", time.Since(started), err)
			}
			if err == nil {
				convergence = append(convergence, time.Since(started).String())
				t.Logf("step %d convergence after %s", len(convergence), convergence[len(convergence)-1])
				observed, e := identity.call("GetUser", rc(f.GroupID))
				require.NoError(t, e)
				names := []string{}
				selected := 0
				for i, row := range observed["userGroup"].([]any) {
					g := row.(map[string]any)
					names = append(names, g["name"].(string))
					if g["id"] == models[0].id {
						selected = i
					}
				}
				checkCLI(t, cli, cliDir, cliEnv, f.Endpoint(harness.App).GRPC, uk, 0, names, selected)
				for _, m := range models {
					code := 0
					if m.status == 4 {
						code = 2
					} else if m.fields["userOwnedKeysEnabled"] != true {
						code = 1
					}
					checkCLI(t, cli, cliDir, cliEnv, f.Endpoint(harness.App).GRPC, m.userKey, code, nil, 0)
				}

				for i := 0; i < 3; i++ {
					time.Sleep(400 * time.Millisecond)
					require.NoError(t, checks(), "converged state regressed")
				}
				return
			}
			if code := status.Code(err); (code != codes.Unknown && code != codes.Unavailable && code != codes.Unauthenticated && code != codes.PermissionDenied) || strings.Contains(err.Error(), "code = InvalidArgument") || strings.Contains(err.Error(), "code = Internal") || strings.Contains(err.Error(), "code = Unimplemented") || strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "context deadline exceeded") {
				require.NoError(t, err, "non-convergence RPC error")
				return
			}
			if time.Now().After(deadline) {
				require.NoError(t, err)
				return
			}
			<-ticker.C
		}
	}
	bools := []string{"sharingEnabled", "useGroupOwnedExecutors", "userOwnedKeysEnabled", "botSuggestionsEnabled", "developerOrgCreationEnabled", "codeSearchEnabled", "restrictCleanWorkflowRunsToAdmins"}
	createOrganization := func(i int, name, slug string) {
		req := rc(f.GroupID)
		req["name"] = name
		req["urlIdentifier"] = slug
		initial := map[string]any{"suggestionPreference": float64(1)}
		for _, k := range bools {
			initial[k] = i == 0
			if k != "restrictCleanWorkflowRunsToAdmins" {
				req[k] = i == 0
			}
		}
		initial["restrictCleanWorkflowRunsToAdmins"] = false
		// Create uses GORM defaults for these proto3 zero values; UpdateGroup can disable them.
		initial["botSuggestionsEnabled"] = true
		initial["developerOrgCreationEnabled"] = true
		// Both groups need a personal credential to exercise the policy transitions.
		initial["userOwnedKeysEnabled"] = true
		req["userOwnedKeysEnabled"] = true
		req["autoPopulateFromOwnedDomain"] = i == 0
		domain := ""
		if i == 0 {
			domain = "example.com"
		}
		created := must(t, identity, "CreateGroup", req)
		id := created["id"].(string)
		key := web("CreateApiKey", map[string]any{"requestContext": f.Web.RequestContext(id), "label": "cache", "capability": []string{"CACHE_WRITE", "ORG_ADMIN", "REGISTER_EXECUTOR"}, "visibleToDevelopers": true})["apiKey"].(map[string]any)["value"].(string)
		personal := web("CreateUserApiKey", map[string]any{"requestContext": f.Web.RequestContext(id), "label": "personal", "capability": []string{"CACHE_WRITE"}})["apiKey"].(map[string]any)
		userKey := personal["value"].(string)
		userKeyID := personal["id"].(string)
		data := []byte("policy-state-" + id)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key).Upload(ctx, "", data)
		require.NoError(t, err)
		_, err = cacheClient(harness.App, key).AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: harness.Digest(data), ActionResult: &repb.ActionResult{StdoutRaw: data}})
		require.NoError(t, err)
		directory, err := cacheClient(harness.App, key).UploadProto(ctx, "", &repb.Directory{Files: []*repb.FileNode{{Name: "policy", Digest: harness.Digest(data)}}})
		require.NoError(t, err)
		command, err := cacheClient(harness.App, key).UploadProto(ctx, "", &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf '%s' \"$1\"", "groups", id}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
		require.NoError(t, err)
		action, err := cacheClient(harness.App, key).UploadProto(ctx, "", &repb.Action{CommandDigest: command, InputRootDigest: directory, Timeout: durationpb.New(5 * time.Second), DoNotCache: true})
		cancel()
		require.NoError(t, err)
		harness.Start(t, harness.Options{Components: []harness.Component{harness.Executor}, AppTarget: "grpc://" + f.Endpoint(harness.App).Container + ":1985", DockerArgs: map[harness.Component][]string{harness.Executor: {"--network", f.Network}}, Config: map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"api_key": key}}}})
		invocationID := seedInvocation(t, cacheClient(harness.App, key), fmt.Sprintf("org%d", i))
		inventory := web("GetApiKeys", rc(id))["apiKey"].([]any)
		developerKey := ""
		for _, row := range inventory {
			k := row.(map[string]any)
			if k["label"] == "Default" {
				r := rc(id)
				r["apiKeyId"] = k["id"]
				developerKey = web("GetApiKey", r)["apiKey"].(map[string]any)["value"].(string)
			}
		}
		require.NotEmpty(t, developerKey)
		models = append(models, &model{id: id, name: name, slug: slug, url: created["url"].(string), domain: domain, fields: initial, status: 1, key: key, userKey: userKey, userKeyID: userKeyID, developerKey: developerKey, invocationID: invocationID, directory: directory, action: action, data: data})
		verify()
	}
	for i := 0; i < 2; i++ {
		createOrganization(i, fmt.Sprintf("Organization %d", i), fmt.Sprintf("organization-%d", i))
	}
	m := models[0]
	for step := 0; step < 4; step++ {
		enabled := step%2 == 0
		m.name = fmt.Sprintf("Renamed %d", step)
		old := m.slug
		m.slug = fmt.Sprintf("renamed-%d", step)
		req := rc(m.id)
		req["id"] = m.id
		req["name"] = m.name
		req["urlIdentifier"] = m.slug
		req["autoPopulateFromOwnedDomain"] = false
		m.domain = ""

		for _, k := range bools {
			req[k] = enabled
			m.fields[k] = enabled
		}
		preference := float64(step%3 + 1)
		req["suggestionPreference"] = preference
		m.fields["suggestionPreference"] = preference
		must(t, client(m.key), "UpdateGroup", req)
		verify()
		must(t, client(m.key), "UpdateGroup", req)
		verify() // valid idempotent update
		if enabled {
			req["autoPopulateFromOwnedDomain"] = true
			web("UpdateGroup", req)
			m.domain = "example.com"
			verify()
			web("UpdateGroup", req)
			verify()
		}
		_, e := client(m.key).call("GetGroup", map[string]any{"urlIdentifier": old})
		require.Equal(t, codes.NotFound, status.Code(e), "old slug should no longer resolve")
		_, e = f.Web.RPC("GetGroup", map[string]any{"urlIdentifier": old})
		require.Error(t, e)
		require.Contains(t, e.Error(), "code = NotFound")
		require.Contains(t, e.Error(), "The requested organization was not found")
	}
	// Restore the personal-key policy so status independently controls API access.
	restore := rc(m.id)
	restore["id"] = m.id
	restore["name"] = m.name
	restore["urlIdentifier"] = m.slug
	for k, v := range m.fields {
		restore[k] = v
	}
	restore["userOwnedKeysEnabled"] = true
	must(t, client(m.key), "UpdateGroup", restore)
	m.fields["userOwnedKeysEnabled"] = true
	verify()
	// A normal server-admin action provisions a disposable impersonation credential.
	impersonation := web("CreateImpersonationApiKey", rc(m.id))["apiKey"].(map[string]any)["value"].(string)

	for _, status := range []float64{1, 2, 3, 5, 6, 4, 1} {
		req := rc(m.id)
		req["status"] = status
		must(t, client(impersonation), "SetGroupStatus", req)
		m.status = status
		verify()
		must(t, client(impersonation), "SetGroupStatus", req)
		verify()
	}
	// There is no DeleteGroup RPC. Reuse a relinquished slug with a new group
	// instead: the original group's identity, membership, keys and cache remain.
	createOrganization(2, "Organization 0", "organization-0")
	reused := must(t, identity, "GetGroup", map[string]any{"urlIdentifier": "organization-0"})
	require.Equal(t, models[2].id, reused["id"])
	require.NotEqual(t, models[0].id, reused["id"])
	// Policy updates retain ordinary authorized cache access on both serving surfaces.
	for _, org := range models {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		app := harness.Dial(t, f.Endpoint(harness.App).GRPC, org.key)
		proxy := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, org.key)
		data := []byte("organization policy roundtrip " + org.id)
		d, e := proxy.Upload(ctx, "", data)
		require.NoError(t, e)
		for _, c := range []*harness.Clients{app, proxy} {
			b, e := c.Read(ctx, "", d)
			require.NoError(t, e)
			require.Equal(t, data, b)
		}
		cancel()
	}
}
