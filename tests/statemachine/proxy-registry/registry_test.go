package registry_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoparse"
	"github.com/jhump/protoreflect/dynamic"
	"google.golang.org/grpc"
)

type rig struct {
	otherGroup     string
	expectedWrites string
	otherConn      *grpc.ClientConn
	t              *testing.T
	f              *harness.Fixture
	conn           *grpc.ClientConn
	fd             *desc.FileDescriptor
}

func (r *rig) msg(name string, v map[string]any) *dynamic.Message {
	r.t.Helper()
	m := dynamic.NewMessage(r.fd.FindMessage("cache_proxy." + name))
	b, e := json.Marshal(v)
	if e != nil {
		r.t.Fatal(e)
	}
	if e = m.UnmarshalJSON(b); e != nil {
		r.t.Fatal(e)
	}
	return m
}
func decode(t *testing.T, m *dynamic.Message) map[string]any {
	t.Helper()
	b, e := m.MarshalJSON()
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func (r *rig) read(method string, extra map[string]any, web bool) (map[string]any, error) {
	req := map[string]any{"requestContext": r.f.Web.RequestContext(r.f.GroupID)}
	for k, v := range extra {
		req[k] = v
	}
	if web {
		return r.f.Web.RPC(method, req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out := r.msg(method+"Response", nil)
	conn := r.conn
	if rc, ok := req["requestContext"].(map[string]any); ok && rc["groupId"] == r.otherGroup {
		conn = r.otherConn
	}
	e := conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, r.msg(method+"Request", req), out)
	if e != nil {
		return nil, fmt.Errorf("%s gRPC: %w", method, e)
	}
	return decode(r.t, out), nil
}
func poll(t *testing.T, timeout time.Duration, check func() error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var last error
	for {
		if last = check(); last == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("poll: %v", last)
		case <-tick.C:
		}
	}
}
func summaries(v map[string]any, method string) []map[string]any {
	key := "summary"
	if method == "GetCacheProxies" {
		key = "cacheProxy"
	}
	var out []map[string]any
	for _, x := range array(v[key]) {
		m := x.(map[string]any)
		if method == "GetCacheProxies" {
			m = m["summary"].(map[string]any)
		}
		out = append(out, m)
	}
	return out
}
func array(v any) []any { a, _ := v.([]any); return a }
func (r *rig) visible(id string, model map[string]any, present bool) error {
	for _, web := range []bool{false, true} {
		for _, method := range []string{"GetCacheProxies", "ListCacheProxies"} {
			other, err := r.read(method, map[string]any{"requestContext": r.f.Web.RequestContext(r.otherGroup)}, web)
			if err != nil {
				return err
			}
			if len(summaries(other, method)) != 0 {
				return fmt.Errorf("registration leaked into other org: %v", other)
			}
			v, e := r.read(method, nil, web)
			if e != nil {
				return e
			}
			if method == "GetCacheProxies" && present && strings.HasPrefix(id, "conformance-") {
				for _, entry := range array(v["cacheProxy"]) {
					m := entry.(map[string]any)
					if m["summary"].(map[string]any)["proxyId"] == id && !reflect.DeepEqual(m["statistics"], map[string]any{"casWrites": r.expectedWrites}) {
						return fmt.Errorf("registry statistics: %v", m)
					}
				}
			}
			found := 0
			for _, s := range summaries(v, method) {
				if s["proxyId"] != id {
					continue
				}
				found++
				for k, want := range model {
					if !reflect.DeepEqual(s[k], want) {
						return fmt.Errorf("%s HTTP=%v %s got %v want %v", method, web, k, s[k], want)
					}
				}
				if s["lastCheckInTime"] == nil {
					return fmt.Errorf("missing check-in")
				}
			}
			if (found == 1) != present || found > 1 {
				return fmt.Errorf("%s HTTP=%v id=%s count=%d present=%v", method, web, id, found, present)
			}
		}
	}
	return nil
}
func TestRegistryStateMachine(t *testing.T) {
	files, e := (&protoparse.Parser{ImportPaths: []string{"."}}).ParseFiles("proto/cache_proxy.proto")
	if e != nil {
		t.Fatal(e)
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	key := f.Web.CreateAPIKey(t, f.GroupID, "registry", []string{"REGISTER_CACHE_PROXY", "CACHE_WRITE", "ORG_ADMIN"})
	r := &rig{otherGroup: f.Web.CreateGroup(t, "registry-other"), t: t, f: f, conn: harness.Dial(t, f.Endpoint(harness.App).GRPC, key).Conn, fd: files[0]}
	// Use the app's unique container name to avoid alias ambiguity across networks.
	r.otherConn = harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, r.otherGroup, "other-reader", []string{"ORG_ADMIN"})).Conn
	target := "grpc://" + f.Endpoint(harness.App).Container + ":1985"
	p := harness.Start(t, harness.Options{Components: []harness.Component{harness.CacheProxy}, AppTarget: target, DockerArgs: map[harness.Component][]string{harness.CacheProxy: {"--network", f.Network}}, Config: map[harness.Component]harness.Config{harness.CacheProxy: {"cache_proxy": harness.Config{"app_target": target, "api_key": key, "labels": map[string]string{"conformance": "registry"}}}}})

	var realID string
	var realModel map[string]any
	poll(t, time.Minute, func() error {
		v, e := r.read("ListCacheProxies", nil, false)
		if e != nil {
			return e
		}
		s := summaries(v, "ListCacheProxies")
		if len(s) != 1 {
			return fmt.Errorf("real proxy count %d", len(s))
		}
		realID, _ = s[0]["proxyId"].(string)
		realModel = s[0]
		delete(realModel, "lastCheckInTime")
		return r.visible(realID, map[string]any{"osFamily": "linux", "arch": "amd64", "labels": map[string]any{"conformance": "registry"}}, true)
	})
	poll(t, time.Minute, func() error { return r.visible(realID, realModel, true) })
	for _, web := range []bool{false, true} {
		v, e := r.read("GetCacheProxy", map[string]any{"selector": map[string]any{"proxyId": realID}, "includeConfiguredFlags": true, "includeStatistics": true}, web)
		if e != nil {
			t.Fatal(e)
		}
		d := v["details"].(map[string]any)
		summary := d["summary"].(map[string]any)
		delete(summary, "lastCheckInTime")
		if !reflect.DeepEqual(summary, realModel) {
			t.Fatalf("live detail identity differs HTTP=%v: %v / %v", web, summary, realModel)
		}
		if !reflect.DeepEqual(d["configuredFlags"], realModel["configuredFlags"]) {
			t.Fatalf("live flags differ HTTP=%v: %v / %v", web, d["configuredFlags"], realModel["configuredFlags"])
		}
		if d["summary"].(map[string]any)["proxyId"] != realID || len(array(d["configuredFlags"])) == 0 || d["statistics"] == nil {
			t.Fatalf("real details: %v", v)
		}
	}

	// Normal local cache traffic changes the statistics reported over registration.
	pc := harness.Dial(t, p.Endpoint(harness.CacheProxy).GRPC, key)
	trafficCtx, trafficCancel := context.WithTimeout(context.Background(), time.Minute)
	digest, err := pc.Write(trafficCtx, "registry", []byte("registry conformance payload"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pc.Read(trafficCtx, "registry", digest); err != nil {
		t.Fatal(err)
	}
	trafficCancel()
	poll(t, time.Minute, func() error {
		for _, web := range []bool{false, true} {
			v, e := r.read("GetCacheProxy", map[string]any{"selector": map[string]any{"proxyId": realID}, "includeStatistics": true}, web)
			if e != nil {
				return e
			}
			stats := v["details"].(map[string]any)["statistics"].(map[string]any)
			if stats["casWrites"] == nil || stats["casWrites"] == "0" {
				return fmt.Errorf("live write statistics: %v", stats)
			}
			old, e := r.read("GetCacheProxies", nil, web)
			if e != nil {
				return e
			}
			for _, entry := range array(old["cacheProxy"]) {
				m := entry.(map[string]any)
				if m["summary"].(map[string]any)["proxyId"] == realID && !reflect.DeepEqual(m["statistics"], stats) {
					return fmt.Errorf("heartbeat/detail statistics differ: %v / %v", m["statistics"], stats)
				}
			}
		}
		return nil
	})
	// This sends SIGTERM only to the fixture's released proxy; its shutdown hook
	// must deliver the goodbye on the live registration stream.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Minute)
	if out, err := exec.CommandContext(stopCtx, "docker", "stop", "--time", "30", p.Endpoint(harness.CacheProxy).Container).CombinedOutput(); err != nil {
		t.Fatalf("stop proxy: %s: %v", out, err)
	}
	stopCancel()
	poll(t, time.Minute, func() error { return r.visible(realID, nil, false) })
	for _, web := range []bool{false, true} {
		if _, err := r.read("GetCacheProxy", map[string]any{"selector": map[string]any{"proxyId": realID}}, web); err == nil {
			t.Fatal("stopped real proxy still serves details")
		}
	}
	for _, expiry := range []bool{false, true} {
		id := fmt.Sprintf("conformance-%v", expiry)
		model := map[string]any{"proxyId": id, "host": "registry-test", "osFamily": "linux", "arch": "amd64", "version": "v2.314.0", "configuredFlags": []any{"--cache_proxy.proxy_type=customer"}}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()
		stream, e := r.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/cache_proxy.CacheProxyRegistry/RegisterAndStreamHeartbeat")
		if e != nil {
			t.Fatal(e)
		}
		send := func(v map[string]any) {
			t.Helper()
			if e := stream.SendMsg(r.msg("RegisterCacheProxyRequest", v)); e != nil {
				t.Fatal(e)
			}
		}
		r.expectedWrites = "7"
		for step := 0; step < 3; step++ {
			if step == 2 {
				model["host"] = "registry-updated"
				r.expectedWrites = "9"
			}
			send(map[string]any{"summary": model, "statistics": map[string]any{"casWrites": r.expectedWrites}})
			poll(t, time.Minute, func() error { return r.visible(id, model, true) })
			// Answer each UI/gRPC detail request through the actual bidirectional stream.
			for _, web := range []bool{false, true} {
				done := make(chan error, 1)
				go func() {
					v, e := r.read("GetCacheProxy", map[string]any{"selector": map[string]any{"proxyId": id}, "includeStatistics": true, "includeConfiguredFlags": true}, web)
					if e == nil {
						d := v["details"].(map[string]any)
						s := d["summary"].(map[string]any)
						for k, w := range model {
							if !reflect.DeepEqual(s[k], w) {
								e = fmt.Errorf("details %s: %v", k, s)
							}
						}
						if !reflect.DeepEqual(d["configuredFlags"], []any{"--cache_proxy.proxy_type=customer"}) {
							e = fmt.Errorf("configured flags: %v", d)
						}
						if !reflect.DeepEqual(d["statistics"], map[string]any{"casWrites": r.expectedWrites}) {
							e = fmt.Errorf("statistics: %v", d)
						}
					}
					done <- e
				}()
				rsp := r.msg("RegisterCacheProxyResponse", nil)
				if e := stream.RecvMsg(rsp); e != nil {
					t.Fatal(e)
				}
				v := decode(t, rsp)
				if v["requestId"] == nil {
					t.Fatal("missing request correlation")
				}
				send(map[string]any{"summary": model, "statistics": map[string]any{"casWrites": r.expectedWrites}, "details": map[string]any{"summary": model, "statistics": map[string]any{"casWrites": r.expectedWrites}, "configuredFlags": []string{"--cache_proxy.proxy_type=customer"}}, "requestId": v["requestId"]})
				if e := <-done; e != nil {
					t.Fatal(e)
				}
			}
		}
		if !expiry {
			send(map[string]any{"summary": model, "shuttingDown": true})
		}
		if e := stream.CloseSend(); e != nil {
			t.Fatal(e)
		}
		if e := stream.RecvMsg(r.msg("RegisterCacheProxyResponse", nil)); e != io.EOF {
			t.Fatalf("closure: %v", e)
		}
		for _, web := range []bool{false, true} {
			if _, e := r.read("GetCacheProxy", map[string]any{"selector": map[string]any{"proxyId": id}}, web); e == nil {
				t.Fatal("closed stream served details")
			}
		}
		if expiry {
			if e := r.visible(id, model, true); e != nil {
				t.Fatal(e)
			}
			t.Log("waiting for released hardcoded 10-minute heartbeat expiry")
		}
		timeout := time.Minute
		if expiry {
			timeout = 11 * time.Minute
		}
		closedAt := time.Now()
		poll(t, timeout, func() error {
			err := r.visible(id, nil, false)
			if expiry && err == nil && time.Since(closedAt) < 9*time.Minute+55*time.Second {
				t.Fatalf("registration expired before released TTL: %s", time.Since(closedAt))
			}
			return err
		})
		for _, web := range []bool{false, true} {
			if _, e := r.read("GetCacheProxy", map[string]any{"selector": map[string]any{"proxyId": id}}, web); e == nil {
				t.Fatalf("closed stream still serves details HTTP=%v", web)
			}
		}
		cancel()
	}
}
