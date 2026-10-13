package iprules_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Run the released bb binary, never build the CLI or components from source.
func loginChecks(t *testing.T, client *harness.Clients, key string) func(string, bool) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("released CLI download HTTP %d", resp.StatusCode)
	}
	binary, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(binary)) != "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9" {
		t.Fatal("released CLI checksum mismatch")
	}
	path := filepath.Join(dir, "bb")
	if err := os.WriteFile(path, binary, 0700); err != nil {
		t.Fatal(err)
	}
	git := exec.Command("git", "init", "--quiet", dir)
	if b, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, b)
	}
	git = exec.Command("git", "-C", dir, "config", "buildbuddy.api-key", key)
	if b, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v %s", err, b)
	}
	// bb login has no forwarded-IP flag. A fixture-local gRPC forwarder attaches
	// the same documented trusted-forwarder IP used by the direct probes.
	return func(ip string, allowed bool) {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(grpc.ForceServerCodec(wireCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			method, _ := grpc.MethodFromServerStream(stream)
			if method != "/buildbuddy.service.BuildBuddyService/GetUser" {
				return status.Error(codes.Unimplemented, "CLI forwarder only permits GetUser")
			}
			var in, out []byte
			if err := stream.RecvMsg(&in); err != nil {
				return err
			}
			callCtx := metadata.AppendToOutgoingContext(stream.Context(), "x-forwarded-for", ip)
			if err := client.Conn.Invoke(callCtx, method, in, &out, grpc.ForceCodec(wireCodec{})); err != nil {
				return err
			}
			return stream.SendMsg(out)
		}))
		go server.Serve(listener)
		defer server.Stop()
		defer listener.Close()
		cc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cc, path, "login", "--check", "--target=grpc://"+listener.Addr().String(), "--url=http://127.0.0.1")
		cmd.Dir = dir
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "BB_CACHE_DIR=" + filepath.Join(dir, "cache")}
		b, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		want := 0
		if !allowed {
			want = 2
		}
		if code != want {
			t.Fatalf("bb 5.0.492 login --check ip=%s allowed=%v exit=%d want=%d output=%s", ip, allowed, code, want, b)
		}
	}
}
