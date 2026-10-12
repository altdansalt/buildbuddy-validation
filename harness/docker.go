package harness

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type dockerDaemon struct {
	dir  string
	cmd  *exec.Cmd
	log  *os.File
	done chan error
}

// Hosted runners contain Docker and passwordless sudo, but do not start dockerd
// by default. If no daemon is running, own one for this fixture (including its
// socket, IPAM pool and data directory); never race over /var/run/docker.sock.
func (f *Fixture) ensureDocker() {
	if _, err := f.command("info"); err == nil {
		return
	}
	// Docker's initial iptables chain creation races across concurrent daemons.
	// Serialize just startup, while containers and tests run independently.
	lock, err := os.OpenFile(filepath.Join(os.TempDir(), "bbval-dockerd-start.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		f.t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	dir, err := os.MkdirTemp("", "bbval-docker-")
	if err != nil {
		f.t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(f.OutputDir, "dockerd.log"))
	if err != nil {
		f.t.Fatal(err)
	}
	d := &dockerDaemon{dir: dir, log: log, done: make(chan error, 1)}
	f.daemon = d
	f.dockerHost = "unix://" + filepath.Join(dir, "docker.sock")
	configFile := filepath.Join(dir, "daemon.json")
	if err := os.WriteFile(configFile, []byte(`{"log-level":"warn"}`), 0600); err != nil {
		f.t.Fatal(err)
	}
	// Assign each daemon a large, random private pool. --bridge=none avoids
	// conflicting default docker0 devices; each fixture creates its own bridge.
	var poolByte [2]byte
	if _, err := rand.Read(poolByte[:]); err != nil {
		f.t.Fatal(err)
	}
	pool := fmt.Sprintf("10.%d.%d.0/24", int(poolByte[0]), int(poolByte[1]))
	args := []string{"-n", "dockerd", "--config-file", configFile, "--host", f.dockerHost, "--data-root", filepath.Join(dir, "data"), "--exec-root", filepath.Join(dir, "exec"), "--pidfile", filepath.Join(dir, "daemon.pid"), "--bridge=none", "--default-address-pool", "base=" + pool + ",size=28"}
	if os.Geteuid() == 0 {
		d.cmd = exec.Command("dockerd", args[2:]...)
	} else {
		d.cmd = exec.Command("sudo", args...)
	}
	d.cmd.Stdout = log
	d.cmd.Stderr = log
	if err := d.cmd.Start(); err != nil {
		f.t.Fatalf("start fixture dockerd (or pass --runner_exec_properties=init-dockerd=true): %v", err)
	}
	go func() { d.done <- d.cmd.Wait() }()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-d.done:
			d.cmd = nil
			b, _ := os.ReadFile(log.Name())
			f.t.Fatalf("dockerd exited: %v\n%s", err, b)
		default:
		}
		if _, err := f.command("info"); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	b, _ := os.ReadFile(log.Name())
	f.t.Fatalf("dockerd readiness timed out\n%s", b)
}
func (d *dockerDaemon) close(t testing.TB) {
	if d.cmd != nil {
		b, err := os.ReadFile(filepath.Join(d.dir, "daemon.pid"))
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil && pid > 1 {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				args := []string{"-n", "kill", "-TERM", strconv.Itoa(pid)}
				cmd := exec.CommandContext(ctx, "sudo", args...)
				if os.Geteuid() == 0 {
					cmd = exec.CommandContext(ctx, "kill", args[2:]...)
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("stop dockerd: %v %s", err, out)
				}
				cancel()
			}
		}
		select {
		case <-d.done:
		case <-time.After(15 * time.Second):
			t.Errorf("dockerd did not stop")
		}
	}
	d.log.Close()
	// dockerd leaves root-owned files. Only remove this randomly created daemon
	// directory, never a caller-provided path or TEST_UNDECLARED_OUTPUTS_DIR.
	if !strings.HasPrefix(d.dir, filepath.Join(os.TempDir(), "bbval-docker-")) {
		t.Errorf("unsafe daemon cleanup path")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "-n", "rm", "-rf", "--", d.dir)
	if os.Geteuid() == 0 {
		cmd = exec.CommandContext(ctx, "rm", "-rf", "--", d.dir)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("remove docker data: %v %s", err, out)
	}
}
