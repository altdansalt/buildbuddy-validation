// Package harness runs released BuildBuddy components in per-test Docker networks.
// It never reads the dispatch API key or inherits host environment into containers.
package harness

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

type Config map[string]any

type Options struct {
	// Components is the exact subset to start. Dependencies aren't implicitly added.
	Components []Component
	Images     map[Component]string
	// Config recursively merges into defaults; Args are appended after --config_file.
	Config map[Component]Config
	Args   map[Component][]string
	// DockerArgs permits area tests to opt into privileged mode, devices or mounts.
	DockerArgs map[Component][]string
	// AppTarget can refer to an app in the fixture or another local fixture.
	AppTarget      string
	StartupTimeout time.Duration
	// SkipAuthBootstrap permits tests to exercise an uninitialized app.
	SkipAuthBootstrap bool
	// ExpectStartupFailure waits for these components to exit unsuccessfully.
	ExpectStartupFailure map[Component]bool
}

type Endpoint struct {
	ServerType   string
	SelfAuthURL  string
	Container    string
	Image        string
	HTTP         string
	GRPC         string // host:port, usable with grpc.NewClient
	InternalGRPC string // grpc://<docker-name>:1985, usable by peer containers
}

type Fixture struct {
	t                 testing.TB
	ID                string
	Network           string
	OutputDir         string
	Endpoints         map[Component]*Endpoint
	APIKey            string
	GroupID           string
	Web               *WebSession
	containers        []string
	dockerHost        string
	daemon            *dockerDaemon
	opts              Options
	appTarget, jwtKey string
	ports             map[Component][2]string
	kinds             map[Component]Component
}

// Start registers cleanup before creating any resource, including on startup failure.
func Start(t testing.TB, opts Options) *Fixture {
	t.Helper()
	f := &Fixture{t: t, ID: "bbval-" + uuid.NewString(), Endpoints: make(map[Component]*Endpoint)}
	f.Network = f.ID
	root := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if root == "" {
		root = t.TempDir()
	}
	f.OutputDir = filepath.Join(root, f.ID)
	if err := os.MkdirAll(f.OutputDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	f.ensureDocker()
	f.docker("network", "create", f.Network)
	if opts.StartupTimeout == 0 {
		opts.StartupTimeout = 2 * time.Minute
	}
	f.opts = opts
	f.ports = make(map[Component][2]string)
	f.kinds = make(map[Component]Component)
	want := make(map[Component]bool)
	for _, c := range opts.Components {
		if _, ok := PinnedImages[c]; !ok {
			t.Fatalf("unknown component %q", c)
		}
		if want[c] {
			t.Fatalf("duplicate component %q", c)
		}
		want[c] = true
	}
	appTarget := opts.AppTarget
	if appTarget == "" {
		port := "1985"
		if value, ok := opts.Config[App]["grpc_port"]; ok {
			port = fmt.Sprint(value)
		}
		for _, arg := range opts.Args[App] {
			if value, ok := strings.CutPrefix(arg, "--grpc_port="); ok {
				port = value
			}
		}
		appTarget = "grpc://" + f.ID + "-app:" + port
	}
	if want[App] {
		name := f.ID + "-redis"
		f.containers = append(f.containers, name)
		f.docker("run", "-d", "--name", name, "--network", f.Network, "--network-alias", "redis", RedisImage, "redis-server", "--save", "", "--appendonly", "no")
		deadline := time.Now().Add(opts.StartupTimeout)
		for {
			out, err := f.command("exec", name, "redis-cli", "ping")
			if err == nil && strings.TrimSpace(string(out)) == "PONG" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Redis readiness: %s (%v)", out, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	jwtKey := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	f.appTarget, f.jwtKey = appTarget, jwtKey
	for _, c := range []Component{App, Executor, CacheProxy} {
		if want[c] {
			f.addComponent(c, c, opts)
		}
	}
	if want[App] && want[Executor] && !opts.SkipAuthBootstrap && !opts.ExpectStartupFailure[App] && !opts.ExpectStartupFailure[Executor] {
		f.WaitExecutionReady()
	}
	return f
}

func (f *Fixture) addComponent(id, c Component, opts Options) *Endpoint {
	t := f.t
	appTarget, jwtKey := f.appTarget, f.jwtKey
	cfg := defaults(c, appTarget, jwtKey)
	cfg["server_type"] = string(c)
	cfg["port"] = 8080
	if c != Executor {
		cfg["grpc_port"] = 1985
	}
	merge(cfg, opts.Config[c])
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"app.buildbuddy.io", "remote.buildbuddy.io"} {
		if strings.Contains(string(data)+strings.Join(opts.Args[c], " "), forbidden) {
			t.Fatalf("cloud component target forbidden: %s", forbidden)
		}
	}
	if c == App {
		appCfg, _ := asMap(cfg["app"])
		overrideApp, _ := asMap(opts.Config[App]["app"])
		if _, explicit := overrideApp["build_buddy_url"]; !explicit {
			port := fmt.Sprint(cfg["port"])
			for _, arg := range opts.Args[App] {
				if value, ok := strings.CutPrefix(arg, "--port="); ok {
					port = value
				}
			}
			appCfg["build_buddy_url"] = "http://app:" + port
			data, err = yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	path := filepath.Join(f.OutputDir, string(id)+".yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	name := f.ID + "-" + string(id)
	image := Image(c, opts.Images)
	f.containers = append(f.containers, name)
	httpPort := fmt.Sprint(cfg["port"])
	grpcPort := fmt.Sprint(cfg["grpc_port"])
	serverType := fmt.Sprint(cfg["server_type"])
	for _, arg := range opts.Args[c] {
		if value, ok := strings.CutPrefix(arg, "--port="); ok {
			httpPort = value
		}
		if value, ok := strings.CutPrefix(arg, "--grpc_port="); ok {
			grpcPort = value
		}
		if value, ok := strings.CutPrefix(arg, "--server_type="); ok {
			serverType = value
		}
	}
	args := []string{"create", "--name", name, "--network", f.Network, "--network-alias", string(c), "--hostname", name, "--platform", "linux/amd64", "--volume", "/data", "-p", "127.0.0.1::" + httpPort}
	if c != Executor {
		args = append(args, "-p", "127.0.0.1::"+grpcPort)
	}
	args = append(args, opts.DockerArgs[c]...)
	args = append(args, image, "--config_file=/config.yaml")
	args = append(args, opts.Args[c]...)
	f.docker(args...)
	f.docker("cp", path, name+":/config.yaml")
	f.docker("start", name)
	ep := &Endpoint{ServerType: serverType, Container: name, Image: image, InternalGRPC: "grpc://" + name + ":" + grpcPort}
	if c == Executor {
		ep.InternalGRPC = ""
	}
	if c == App {
		appCfg, _ := asMap(cfg["app"])
		ep.SelfAuthURL = fmt.Sprint(appCfg["build_buddy_url"])
	}
	f.Endpoints[id] = ep
	f.ports[id] = [2]string{httpPort, grpcPort}
	f.kinds[id] = c
	if opts.ExpectStartupFailure[c] {
		f.WaitExit(id, true)
		return ep
	}
	f.refresh(id)
	if c == App && !opts.SkipAuthBootstrap {
		f.Web = LoginSelfAuth(t, ep)
		f.GroupID = f.Web.GroupID
		f.APIKey = f.Web.CreateAPIKey(t, f.GroupID, "fixture", []string{"CACHE_WRITE", "REGISTER_EXECUTOR"})
	}
	t.Logf("%s: image=%s HTTP=%s gRPC=%s", c, image, ep.HTTP, ep.GRPC)
	return ep
}

func defaults(c Component, appTarget, jwtKey string) Config {
	switch c {
	case App:
		return Config{
			"disable_telemetry": true,
			"telemetry_port":    -1,
			"app":               Config{"build_buddy_url": "http://app:8080", "default_redis_target": "redis:6379", "no_default_user_group": true, "create_group_per_user": true},
			"database":          Config{"data_source": "sqlite3:///data/app.db"},
			"storage":           Config{"disk": Config{"root_directory": "/data/storage"}},
			"cache":             Config{"max_size_bytes": int64(1 << 30), "pebble": Config{"root_directory": "/data/cache"}},
			"auth":              Config{"enable_anonymous_usage": true, "enable_self_auth": true, "jwt_key": "fixture-only-hmac-key", "jwt_es256_private_key": jwtKey},
			"remote_execution":  Config{"enable_remote_exec": true, "require_executor_authorization": false},
			"ssl":               Config{"enable_ssl": false},
		}
	case Executor:
		return Config{"executor": Config{"app_target": appTarget, "root_directory": "/data/executor", "local_cache_directory": "/data/filecache", "local_cache_size_bytes": int64(1 << 30), "enable_bare_runner": true, "default_isolation_type": "none"}, "monitoring_port": 9090}
	case CacheProxy:
		return Config{
			"cache_proxy": Config{"remote_cache": appTarget, "proxy_type": "customer"},
			"cache":       Config{"max_size_bytes": int64(1 << 30), "pebble": Config{"root_directory": "/data/cache"}},
			"auth":        Config{"remote": Config{"target": appTarget, "use_es256_jwts": true}, "reparse_jwts": false},
			"ssl":         Config{"enable_ssl": false},
		}
	}
	panic("unknown component")
}

func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case Config:
		return m, true
	case map[string]any:
		return m, true
	}
	return nil, false
}
func merge(dst Config, src Config) {
	for k, v := range src {
		a, aok := asMap(dst[k])
		b, bok := asMap(v)
		if aok && bok {
			merge(Config(a), Config(b))
		} else {
			dst[k] = v
		}
	}
}
func (f *Fixture) command(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if f.dockerHost != "" {
		args = append([]string{"--host", f.dockerHost}, args...)
	}
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}
func (f *Fixture) docker(args ...string) string {
	f.t.Helper()
	b, err := f.command(args...)
	if err != nil {
		f.t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, b)
	}
	return strings.TrimSpace(string(b))
}
func (f *Fixture) port(name, port string) string {
	b := f.docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", name)
	var ports map[string][]struct {
		HostIP   string
		HostPort string
	}
	if err := json.Unmarshal([]byte(b), &ports); err != nil {
		f.t.Fatal(err)
	}
	p := ports[port+"/tcp"]
	if len(p) == 0 {
		f.t.Fatalf("no mapping for %s:%s", name, port)
	}
	return "127.0.0.1:" + p[0].HostPort
}
func (f *Fixture) waitReady(c Component, ep *Endpoint, timeout time.Duration) {
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get(ep.HTTP + "/readyz?server-type=" + ep.ServerType)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		state, err := f.command("inspect", "--format", "{{.State.Running}}", ep.Container)
		if err != nil || strings.TrimSpace(string(state)) != "true" {
			f.t.Fatalf("%s exited during startup: %s\n%s", c, last, f.docker("logs", ep.Container))
		}
		time.Sleep(250 * time.Millisecond)
	}
	f.t.Fatalf("%s readiness timed out: %s\n%s", c, last, f.docker("logs", ep.Container))
}
func (f *Fixture) Endpoint(c Component) *Endpoint {
	f.t.Helper()
	ep := f.Endpoints[c]
	if ep == nil {
		f.t.Fatalf("component %s not started", c)
	}
	return ep
}

// Close preserves logs and inspect metadata before removing every owned resource.
// It is safe to call before the registered cleanup runs.
func (f *Fixture) Close() {
	for i := len(f.containers) - 1; i >= 0; i-- {
		name := f.containers[i]
		for _, cmd := range []string{"logs", "inspect"} {
			b, err := f.command(cmd, name)
			if err != nil {
				b = append(b, []byte(fmt.Sprintf("\n%v\n", err))...)
			}
			path := filepath.Join(f.OutputDir, name+"."+cmd+".txt")
			if err := os.WriteFile(path, b, 0600); err != nil {
				f.t.Errorf("save %s: %v", cmd, err)
			}
		}
		if b, err := f.command("rm", "-f", "-v", name); err != nil {
			f.t.Errorf("remove %s: %v %s", name, err, b)
		}
	}
	f.containers = nil
	if f.Network != "" {
		if b, err := f.command("network", "rm", f.Network); err != nil {
			f.t.Errorf("remove network: %v %s", err, b)
		}
		f.Network = ""
	}
	if f.daemon != nil {
		f.daemon.close(f.t)
		f.daemon = nil
	}
}
