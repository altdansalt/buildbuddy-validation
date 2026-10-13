package membership

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Pin the released CLI independently of the enterprise images. The download
// is tooling only; every CLI RPC uses the fixture-local target and local key.
func downloadCLI(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("CLI download: %s", res.Status)
	}
	path := filepath.Join(t.TempDir(), "bb")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0700)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, hash), res.Body)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9" {
		t.Fatal("CLI checksum mismatch")
	}
	return path
}

func checkCLI(t *testing.T, binary, target string, id *identity, m model) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Python's standard-library PTY gives the real interactive CLI a terminal.
	// Stop after its first complete organization/list frame, then send ctrl+c.
	const driver = `import os,pty,select,sys,time,fcntl,termios,struct
pid,fd=pty.fork()
if pid==0:
 os.execv(sys.argv[1],[sys.argv[1],"ui","--target="+sys.argv[2]])
fcntl.ioctl(fd,termios.TIOCSWINSZ,struct.pack("HHHH",40,120,0,0))
output=b""; deadline=time.monotonic()+10
try:
 while time.monotonic()<deadline:
  if select.select([fd],[],[],0.1)[0]:
   output+=os.read(fd,65536)
   if b"ctrl+c: quit" in output or b"q: quit" in output: break
finally:
 os.write(fd,b"\x03")
 os.waitpid(pid,0)
sys.stdout.buffer.write(output)
`
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-c", driver, binary, target)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "TERM=xterm-256color", "BUILDBUDDY_API_KEY=" + id.key}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bb ui: %v %s", err, out)
	}
	if m.status == 1 {
		if !strings.Contains(string(out), "Select Organization") || !strings.Contains(string(out), "Membership target") || !strings.Contains(string(out), "member home") {
			t.Fatalf("bb ui missing organization projection: %s", out)
		}
	} else if !strings.Contains(string(out), "BuildBuddy Builds") || strings.Contains(string(out), "Membership target") || strings.Contains(string(out), "Select Organization") {
		t.Fatalf("bb ui nonmember projection: %s", out)
	}
}
