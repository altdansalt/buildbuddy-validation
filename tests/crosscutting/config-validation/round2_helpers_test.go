package configvalidation

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

func replaceSetting(t *testing.T, original, old, value string) string {
	t.Helper()
	if strings.Count(original, old) != 1 {
		t.Fatalf("expected exactly one %q in config", old)
	}
	return strings.Replace(original, old, value, 1)
}

func assertStartupRejected(t *testing.T, f *harness.Fixture, d docker, c harness.Component, since, diagnostic string) {
	t.Helper()
	name := f.Endpoint(c).Container
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && d.run(t, "inspect", "--format", "{{.State.Running}}", name) == "true" {
		time.Sleep(200 * time.Millisecond)
	}
	logs := d.run(t, "logs", "--since", since, name)
	t.Logf("startup logs:\n%s", logs)
	if d.run(t, "inspect", "--format", "{{.State.Running}}", name) == "true" {
		t.Fatal("invalid config remained running")
	}
	if strings.Contains(logs, "panic:") {
		t.Error("invalid config caused a panic instead of a configuration error")
	}
	if !strings.Contains(logs, diagnostic) {
		t.Errorf("error must identify %q", diagnostic)
	}
	if d.run(t, "inspect", "--format", "{{.State.ExitCode}}", name) == "0" {
		t.Error("invalid config exited successfully")
	}
}

func reloadConfig(t *testing.T, f *harness.Fixture, d docker, c harness.Component, config string, rejected bool) {
	t.Helper()
	name := f.Endpoint(c).Container
	path := filepath.Join(f.OutputDir, string(c)+"-reload.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	d.run(t, "cp", path, name+":/config.yaml")
	since := time.Now().UTC().Format(time.RFC3339Nano)
	d.run(t, "kill", "--signal=HUP", name)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		logs := d.run(t, "logs", "--since", since, name)
		if strings.Contains(logs, "panic:") {
			t.Fatalf("reload panicked:\n%s", logs)
		}
		if rejected && strings.Contains(logs, "SIGHUP handler err:") {
			t.Logf("rejected reload:\n%s", logs)
			return
		}
		if !rejected && strings.Contains(logs, "Re-reading buildbuddy config") {
			// This log precedes parsing. The caller polls the observable HTTP change.
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("reload did not produce expected diagnostic:\n%s", d.run(t, "logs", "--since", since, name))
}

func hasCSP(t *testing.T, f *harness.Fixture) bool {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(f.Endpoint(harness.App).HTTP + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status=%d", resp.StatusCode)
	}
	return resp.Header.Get("Content-Security-Policy") != ""
}

func waitCSP(t *testing.T, f *harness.Fixture, want bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if hasCSP(t, f) == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Content-Security-Policy presence never became %v", want)
}

// Extra ports are published only to runner loopback, just like harness ports.
func publishedAddress(t *testing.T, f *harness.Fixture, d docker, port string) string {
	t.Helper()
	return d.run(t, "port", f.Endpoint(harness.App).Container, port+"/tcp")
}

func tlsHandshake(address string) error {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	// The self-signed certificate is generated inside this disposable fixture.
	conn, err := tls.DialWithDialer(dialer, "tcp", address, &tls.Config{InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
	}
	return err
}

func waitReloadMarker(t *testing.T, address, marker string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + address + "/flagz")
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == 200 && strings.Contains(string(body), marker) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("live flags did not contain reload marker %q", marker)
}
