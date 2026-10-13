package groups_test

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/jhump/protoreflect/desc/protoparse"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
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
	files, err := c.ParseFiles("proto/grp.proto", "proto/user.proto", "proto/bazel_config.proto")
	require.NoError(t, err)
	result := map[string]protoreflect.MessageDescriptor{}
	for _, f := range files {
		for i := 0; i < f.UnwrapFile().Messages().Len(); i++ {
			m := f.UnwrapFile().Messages().Get(i)
			result[string(m.Name())] = m
		}
	}
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
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"no_default_user_group": false, "create_group_per_user": false, "user_owned_keys_enabled": true}, "auth": harness.Config{"admin_group_id": "GR0000000000000000000", "api_key_group_cache_ttl": "1s"}}, harness.CacheProxy: {"auth": harness.Config{"remote": harness.Config{"jwt_cache_ttl": "1s"}}}}})
	ds := descriptors(t)
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
		id, name, slug, domain, url string
		fields                      map[string]any
		status                      float64
		key                         string
		userKey                     string
		data                        []byte
		config                      map[string]any
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
			if b["selectedGroup"].(map[string]any)["groupId"] != m.id {
				return fmt.Errorf("HTTP selected organization differs")
			}
			if a["selectedGroup"].(map[string]any)["groupId"] != f.GroupID {
				return fmt.Errorf("gRPC selected organization differs")
			}
			for _, surface := range []map[string]any{a, b} {
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
					if !reflect.DeepEqual(actual, v) {
						return fmt.Errorf("%s: %v != %v", k, actual, v)
					}
				}
				if num(g["status"]) != m.status {
					return fmt.Errorf("status mismatch: %v != %v", g["status"], m.status)
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
				if !reflect.DeepEqual(gr, hr) {
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
				if !reflect.DeepEqual(gc["configOption"], hc["configOption"]) {
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
						if !reflect.DeepEqual(visible[k["id"].(string)], v) {
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
			// Config options do not depend on these policy fields; credential list does.
			if m.config != nil && !reflect.DeepEqual(m.config["options"], hc["configOption"]) {
				return fmt.Errorf("config options changed")
			}
			m.config = map[string]any{"options": hc["configOption"]}
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				data, err := cacheClient(component, m.userKey).Read(ctx, "", harness.Digest(m.data))
				cancel()
				if allowed {
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(data, m.data) {
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
	verify := func() {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var err error
		for {
			err = checks()
			if err == nil {
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
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("Organization %d", i)
		slug := fmt.Sprintf("organization-%d", i)
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
		key := web("CreateApiKey", map[string]any{"requestContext": f.Web.RequestContext(id), "label": "cache", "capability": []string{"CACHE_WRITE", "ORG_ADMIN"}, "visibleToDevelopers": true})["apiKey"].(map[string]any)["value"].(string)
		userKey := web("CreateUserApiKey", map[string]any{"requestContext": f.Web.RequestContext(id), "label": "personal", "capability": []string{"CACHE_WRITE"}})["apiKey"].(map[string]any)["value"].(string)
		data := []byte("policy-state-" + id)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key).Upload(ctx, "", data)
		cancel()
		require.NoError(t, err)
		models = append(models, &model{id: id, name: name, slug: slug, url: created["url"].(string), domain: domain, fields: initial, status: 1, key: key, userKey: userKey, data: data})
		verify()
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
		require.Error(t, e, "old slug should no longer resolve")
		_, e = f.Web.RPC("GetGroup", map[string]any{"urlIdentifier": old})
		require.Error(t, e)
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
	m.config = nil // Provisioning a credential legitimately changes GetBazelConfig.
	for _, status := range []float64{1, 2, 3, 5, 6, 4, 1} {
		req := rc(m.id)
		req["status"] = status
		must(t, client(impersonation), "SetGroupStatus", req)
		m.status = status
		verify()
		must(t, client(impersonation), "SetGroupStatus", req)
		verify()
	}
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
