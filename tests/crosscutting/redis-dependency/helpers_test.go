package redisdependency_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// dockerHost discovers only the daemon owning this fixture. The harness does not
// expose its private daemon socket; this also works without init-dockerd=true.
func dockerHost(t *testing.T, f *harness.Fixture) string {
	t.Helper()
	sockets, _ := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	hosts := append([]string{""}, sockets...)
	for _, host := range hosts {
		args := []string{}
		if host != "" {
			args = append(args, "--host", "unix://"+host)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err := exec.CommandContext(ctx, "docker", append(args, "inspect", "--format", "{{.Name}}", f.ID+"-redis")...).CombinedOutput()
		cancel()
		if err == nil && strings.TrimSpace(string(out)) == "/"+f.ID+"-redis" {
			return host
		}
	}
	t.Fatal("cannot find fixture-owned Redis daemon")
	return ""
}
func docker(t *testing.T, host string, args ...string) string {
	t.Helper()
	if host != "" {
		args = append([]string{"--host", "unix://" + host}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v: %s", args, err, out)
	}
	return string(out)
}

func action(t *testing.T, ctx context.Context, c *harness.Clients, token string, seconds int) *repb.Digest {
	t.Helper()
	root, err := c.UploadProto(ctx, "redis", &repb.Directory{})
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := c.UploadProto(ctx, "redis", &repb.Command{
		Arguments:   []string{"/bin/sh", "-c", fmt.Sprintf("printf x >> /probe/%s; sleep %d; printf '%s' > result", token, seconds, token)},
		OutputPaths: []string{"result"},
		Platform:    &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.UploadProto(ctx, "redis", &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(20 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// NOT_FOUND is expressly retriable through Execute in REAPI WaitExecution.
func retriable(code codes.Code) bool {
	return code == codes.Unavailable || code == codes.NotFound || code == codes.Aborted || code == codes.ResourceExhausted
}
func execute(t *testing.T, ctx context.Context, c *harness.Clients, d *repb.Digest, token string, admitted chan<- string) error {
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "redis", ActionDigest: d, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	if err != nil {
		admitted <- ""
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		admitted <- ""
		return err
	}
	admitted <- first.Name
	if first.Done {
		return fmt.Errorf("unexpected immediate completion")
	}
	for {
		op, err := stream.Recv()
		if err != nil {
			return err
		}
		if op.Name != first.Name {
			return fmt.Errorf("operation name changed")
		}
		if !op.Done {
			continue
		}
		if op.GetError() != nil {
			return fmt.Errorf("REAPI forbids Operation.error: %v", op.GetError())
		}
		r := &repb.ExecuteResponse{}
		if err := op.GetResponse().UnmarshalTo(r); err != nil {
			return err
		}
		if err := validate(ctx, c, r, token); err != nil {
			return err
		}
		if _, err := stream.Recv(); err != io.EOF {
			return fmt.Errorf("after terminal response, expected EOF: %v", err)
		}
		return nil
	}
}

func validate(ctx context.Context, c *harness.Clients, r *repb.ExecuteResponse, token string) error {
	if r.GetStatus().GetCode() != 0 {
		return status.Error(codes.Code(r.Status.Code), r.Status.Message)
	}
	if r.Result == nil || r.Result.ExitCode != 0 || len(r.Result.OutputFiles) != 1 {
		return fmt.Errorf("invalid successful result: %v", r)
	}
	b, err := c.Read(ctx, "redis", r.Result.OutputFiles[0].Digest)
	if err != nil {
		return err
	}
	if string(b) != token {
		return fmt.Errorf("success without expected output: %q", b)
	}
	return nil
}
