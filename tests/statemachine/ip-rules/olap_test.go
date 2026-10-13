package iprules_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The optional OLAP dependency is owned by this test. No shared harness changes
// and no worker-side component execution are needed.
func startOLAP(t *testing.T) string {
	t.Helper()
	docker := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}
	if _, err := docker("info"); err != nil {
		dir, err := os.MkdirTemp("", "ip-rules-dockerd-")
		if err != nil {
			t.Fatal(err)
		}
		// dockerd runs as root, so its files need root to remove; this runs last.
		t.Cleanup(func() { exec.Command("sudo", "-n", "rm", "-rf", dir).Run() })
		host := "unix://" + filepath.Join(dir, "docker.sock")
		t.Setenv("DOCKER_HOST", host)
		lock, err := os.OpenFile(filepath.Join(os.TempDir(), "bbval-dockerd-start.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		logPath := filepath.Join(os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"), "ip-rules-dockerd.log")
		log, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		args := []string{"-n", "dockerd", "--host", host, "--data-root", filepath.Join(dir, "data"), "--exec-root", filepath.Join(dir, "exec"), "--pidfile", filepath.Join(dir, "pid"), "--bridge=none", "--default-address-pool", fmt.Sprintf("base=10.%d.%d.0/24,size=28", sha256.Sum256([]byte(dir))[0], sha256.Sum256([]byte(dir))[1])}
		cmd := exec.Command("sudo", args...)
		cmd.Stdout = log
		cmd.Stderr = log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			pid, readErr := os.ReadFile(filepath.Join(dir, "pid"))
			if readErr == nil {
				exec.Command("sudo", "-n", "kill", "-TERM", strings.TrimSpace(string(pid))).Run()
			} else {
				exec.Command("sudo", "-n", "kill", "-TERM", fmt.Sprint(cmd.Process.Pid)).Run()
			}
			cmd.Wait()
			log.Close()
		})
		deadline := time.Now().Add(40 * time.Second)
		for {
			if _, err := docker("info"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("OLAP dockerd readiness timeout")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	name := fmt.Sprintf("ip-rules-olap-%d", time.Now().UnixNano())
	run := func(args ...string) string {
		t.Helper()
		b, err := docker(args...)
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	run("network", "create", name)
	t.Cleanup(func() { docker("network", "rm", name) })
	const image = "mirror.gcr.io/clickhouse/clickhouse-server@sha256:b627d7a9bc0e0c1bac26cdbe9d2fc6316faa29c5d8a174f28f5abd57d0fa6ba2"
	run("run", "-d", "--name", name, "--network", name, "--network-alias", "ip-rules-olap", "--env", "CLICKHOUSE_DB=buildbuddy_test", "--env", "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1", image)
	t.Cleanup(func() {
		inspect, _ := docker("inspect", name)
		os.WriteFile(filepath.Join(os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"), "ip-rules-clickhouse.inspect.json"), inspect, 0600)
		b, _ := docker("logs", name)
		os.WriteFile(filepath.Join(os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"), "ip-rules-clickhouse.log"), b, 0600)
		docker("rm", "-f", "-v", name)
	})
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := docker("exec", name, "clickhouse-client", "--host", "ip-rules-olap", "--query", "SELECT 1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ClickHouse readiness timeout")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("OLAP image=%s", image)
	return name
}
