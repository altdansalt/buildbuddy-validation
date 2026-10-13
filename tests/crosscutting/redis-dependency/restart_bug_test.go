package redisdependency_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/status"
)

// Shrunk from seed=314: one running action, one queued action, one Redis restart.
// No duplicate requests, executor pause, random inputs, or availability monitor.
func TestQueuedExecutionTerminatesAfterRedisRestart(t *testing.T) {
	t.Log("fixed reproducer shrunk from seed=314")
	probe := t.TempDir()
	if err := os.Chmod(probe, 0777); err != nil {
		t.Fatal(err)
	}
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.Executor},
		Config:     map[harness.Component]harness.Config{harness.Executor: {"executor": harness.Config{"max_concurrent_tasks": 1}}},
		DockerArgs: map[harness.Component][]string{harness.Executor: {"--mount", "type=bind,src=" + probe + ",dst=/probe"}},
	})
	host := dockerHost(t, f)
	c := f.Clients(harness.App)
	t.Cleanup(func() {
		if t.Failed() {
			for _, component := range []harness.Component{harness.App, harness.Executor} {
				t.Logf("%s logs:\n%s", component, docker(t, host, "logs", "--tail", "40", f.Endpoint(component).Container))
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	running := action(t, ctx, c, "running", 5)
	queued := action(t, ctx, c, "queued", 0)
	admitted := make(chan string, 1)
	runningResult := make(chan error, 1)
	go func() { runningResult <- execute(t, ctx, c, running, "running", admitted) }()
	if <-admitted == "" {
		t.Fatal("running action not admitted")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(probe, "running")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("running action never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "redis", ActionDigest: queued, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	if err != nil {
		t.Fatal(err)
	}
	op, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if op.Done || op.Name == "" {
		t.Fatal("queued action not admitted")
	}
	if _, err := os.Stat(filepath.Join(probe, "queued")); !os.IsNotExist(err) {
		t.Fatal("second action was not queued")
	}
	t.Logf("queued operation=%s", op.Name)
	wait, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: op.Name})
	if err != nil {
		t.Fatal(err)
	}
	waitResult := make(chan error, 1)
	go func() {
		for {
			op, err := wait.Recv()
			if err != nil {
				waitResult <- err
				return
			}
			if op.Done {
				if op.GetError() != nil {
					t.Errorf("forbidden Operation.error: %v", op.GetError())
				}
				r := &repb.ExecuteResponse{}
				err := op.GetResponse().UnmarshalTo(r)
				if err == nil {
					err = validate(ctx, c, r, "queued")
				}
				waitResult <- err
				return
			}
		}
	}()
	docker(t, host, "restart", "--time", "0", f.ID+"-redis")
	if got := strings.TrimSpace(docker(t, host, "exec", f.ID+"-redis", "redis-cli", "ping")); got != "PONG" {
		t.Fatalf("Redis recovery failed: %q", got)
	}
	var executionErr error
	for {
		op, err := stream.Recv()
		if err != nil {
			executionErr = err
			break
		}
		if op.Done {
			if op.GetError() != nil {
				t.Errorf("forbidden Operation.error: %v", op.GetError())
			}
			r := &repb.ExecuteResponse{}
			executionErr = op.GetResponse().UnmarshalTo(r)
			if executionErr == nil {
				executionErr = validate(ctx, c, r, "queued")
			}
			break
		}
	}
	if executionErr != nil && !retriable(status.Code(executionErr)) {
		t.Errorf("queued Execute never received completion/retriable failure after Redis recovery: %v", executionErr)
	}
	if err := <-waitResult; err != nil && !retriable(status.Code(err)) {
		t.Errorf("queued WaitExecution never received completion/retriable failure after Redis recovery: %v", err)
	}
	if err := <-runningResult; err != nil && !retriable(status.Code(err)) {
		t.Errorf("running action: %v", err)
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer recoveryCancel()
	rd := action(t, recoveryCtx, c, "recovered", 0)
	a := make(chan string, 1)
	if err := execute(t, recoveryCtx, c, rd, "recovered", a); err != nil {
		t.Fatalf("fresh action did not recover: %v", err)
	}
	t.Log("fresh action succeeded after Redis recovery")
}
