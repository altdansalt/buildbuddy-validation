package configvalidation

import (
	"context"
	"encoding/json"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var components = []harness.Component{harness.App, harness.Executor, harness.CacheProxy}

// The harness has no expected-startup-failure API. Start known-good containers,
// then replace only their YAML and restart them, retaining fixture ownership,
// local peers, image selection and cleanup. Locate an owned daemon by asking
// each socket whether it owns this fixture's container; never manipulate peers.
type docker struct{ prefix []string }

func fixtureDocker(t *testing.T, f *harness.Fixture) docker {
	t.Helper()
	sockets, _ := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	candidates := [][]string{nil}
	for _, s := range sockets {
		candidates = append(candidates, []string{"--host", "unix://" + s})
	}
	for _, p := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := exec.CommandContext(ctx, "docker", append(append([]string{}, p...), "inspect", f.Endpoint(harness.App).Container)...).Run()
		cancel()
		if err == nil {
			return docker{p}
		}
	}
	t.Fatal("could not locate fixture Docker daemon")
	return docker{}
}
func (d docker) run(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", append(append([]string{}, d.prefix...), args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

var portLine = regexp.MustCompile(`(?m)^port: [^\n]+$`)

func restartConfig(t *testing.T, f *harness.Fixture, d docker, c harness.Component, config string) string {
	t.Helper()
	name := f.Endpoint(c).Container
	d.run(t, "stop", "--time", "3", name)
	path := filepath.Join(f.OutputDir, string(c)+"-variation.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	d.run(t, "cp", path, name+":/config.yaml")
	since := time.Now().UTC().Format(time.RFC3339Nano)
	d.run(t, "start", name)
	// Docker may choose different ephemeral host ports on every start.
	var ports map[string][]struct {
		HostIP   string
		HostPort string
	}
	if err := json.Unmarshal([]byte(d.run(t, "inspect", "--format", "{{json .NetworkSettings.Ports}}", name)), &ports); err != nil {
		t.Fatal(err)
	}
	if bindings := ports["8080/tcp"]; len(bindings) > 0 {
		f.Endpoint(c).HTTP = "http://127.0.0.1:" + bindings[0].HostPort
	}
	return since
}
func baseline(t *testing.T, f *harness.Fixture, c harness.Component) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.OutputDir, string(c)+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
