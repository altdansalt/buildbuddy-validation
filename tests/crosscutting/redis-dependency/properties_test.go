package redisdependency_test

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/status"
)

var seed = flag.Int64("redis.seed", 314, "Replay seed")
var queued = flag.Bool("redis.queue", true, "Submit a distinct action to the single-slot executor")
var faultOverride = flag.String("redis.fault", "", "Override fault: pause, stop, restart, none")
var iterations = flag.Int("redis.iterations", 6, "Fault rounds (1..9)")

func TestRedisFaultProperties(t *testing.T)        { redisProperties(t, true) }
func TestDefaultRedisPauseProperties(t *testing.T) { redisProperties(t, false) }
func redisProperties(t *testing.T, monitoring bool) {
	t.Logf("seed=%d iterations=%d queued=%t monitor=%t", *seed, *iterations, *queued, monitoring)
	if *iterations < 1 || *iterations > 9 {
		t.Fatal("iterations must be 1..9")
	}
	switch *faultOverride {
	case "", "pause", "stop", "restart", "none":
	default:
		t.Fatal("invalid redis.fault")
	}
	rng := rand.New(rand.NewSource(*seed))
	probe := t.TempDir()
	if err := os.Chmod(probe, 0777); err != nil {
		t.Fatal(err)
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{harness.App: {"remote_execution": harness.Config{"enable_redis_availability_monitoring": monitoring}}, harness.Executor: {"executor": harness.Config{"max_concurrent_tasks": 1}}}, DockerArgs: map[harness.Component][]string{harness.Executor: {"--mount", "type=bind,src=" + probe + ",dst=/probe"}}})
	host := dockerHost(t, f)
	t.Cleanup(func() {
		if t.Failed() {
			for _, component := range []harness.Component{harness.App, harness.Executor} {
				t.Logf("%s logs:\n%s", component, docker(t, host, "logs", "--tail", "100", f.Endpoint(component).Container))
			}
		}
	})
	redis := f.ID + "-redis"
	c := f.Clients(harness.App)
	for i := 0; i < *iterations; i++ {
		fault := []string{"pause", "stop", "restart"}[i%3]
		if !monitoring {
			fault = "pause"
		}
		if *faultOverride != "" {
			fault = *faultOverride
		}
		t.Run(fmt.Sprintf("%d-%s", i, fault), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
			defer cancel()
			token := fmt.Sprintf("s%d-r%d-%08x", *seed, i, rng.Uint32())
			d := action(t, ctx, c, token, 5+rng.Intn(3))
			admitted := make(chan string, 3)
			results := make(chan error, 3)
			go func() { results <- execute(t, ctx, c, d, token, admitted) }()
			name := <-admitted
			if name == "" {
				t.Fatalf("baseline admission failed: %v", <-results)
			}
			// Observe a real command side effect before injecting the fault.
			deadline := time.Now().Add(15 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(probe, token)); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("action never started before fault")
				}
				time.Sleep(50 * time.Millisecond)
			}
			// Duplicate requests exercise pending action merging; WaitExecution observes
			// the same operation through an independently established stream.
			go func() { results <- execute(t, ctx, c, d, token, admitted) }()
			<-admitted
			wait, err := c.Execution.WaitExecution(ctx, &repb.WaitExecutionRequest{Name: name})
			if err != nil {
				t.Fatal(err)
			}
			waitResult := make(chan error, 1)
			waitReady := make(chan struct{})
			go func() {
				first := true
				for {
					op, err := wait.Recv()
					if first {
						close(waitReady)
						first = false
					}
					if err != nil {
						waitResult <- err
						return
					}
					if op.Done {
						if op.GetError() != nil || op.Name != name {
							waitResult <- fmt.Errorf("invalid WaitExecution operation: %v", op)
							return
						}
						r := &repb.ExecuteResponse{}
						err := op.GetResponse().UnmarshalTo(r)
						if err == nil {
							err = validate(ctx, c, r, token)
						}
						if err == nil {
							if _, eof := wait.Recv(); eof != io.EOF {
								err = fmt.Errorf("after terminal WaitExecution, expected EOF: %v", eof)
							}
						}
						waitResult <- err
						return
					}
				}
			}()
			<-waitReady
			// A single execution slot queues a distinct action without interrupting
			// executor registration, task admission, or its lease renewals.
			count := 2
			if *queued {
				queuedToken := token + "-queued"
				qd := action(t, ctx, c, queuedToken, 0)
				go func() { results <- execute(t, ctx, c, qd, queuedToken, admitted) }()
				if name := <-admitted; name == "" {
					t.Fatal("queued action not admitted")
				}
				if _, err := os.Stat(filepath.Join(probe, queuedToken)); !os.IsNotExist(err) {
					t.Fatal("queued action started before fault; increase command duration")
				}
				count++
			}

			hold := time.Duration(250+rng.Intn(1000)) * time.Millisecond
			t.Logf("seed=%d token=%s fault=%s duration=%s operation=%s", *seed, token, fault, hold, name)
			switch fault {
			case "pause":
				docker(t, host, "pause", redis)
				time.Sleep(hold)
				docker(t, host, "unpause", redis)
			case "stop":
				docker(t, host, "stop", "--time", "0", redis)
				time.Sleep(hold)
				docker(t, host, "start", redis)
			case "restart":
				docker(t, host, "restart", "--time", "0", redis)
			}
			if got := strings.TrimSpace(docker(t, host, "exec", redis, "redis-cli", "ping")); got != "PONG" {
				t.Fatalf("Redis did not recover: %q", got)
			}

			for n := 0; n < count; n++ {
				err := <-results
				if err != nil && !retriable(status.Code(err)) {
					t.Errorf("seed=%d Execute: %v", *seed, err)
				}
			}
			if err := <-waitResult; err != nil && !retriable(status.Code(err)) {
				t.Errorf("seed=%d WaitExecution: %v", *seed, err)
			}
			b, err := os.ReadFile(filepath.Join(probe, token))
			if err != nil || len(b) != 1 {
				t.Errorf("seed=%d action ran %d times (REAPI permits retries, but exactly-once application invariant failed): %v", *seed, len(b), err)
			}
			// A fresh action must execute after Redis is restored, regardless of whether
			// older operations completed or received a retriable failure.
			ctx, recoveryCancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer recoveryCancel()
			recovery := token + "-recovery"
			rd := action(t, ctx, c, recovery, 0)
			a := make(chan string, 1)
			if err := execute(t, ctx, c, rd, recovery, a); err != nil {
				t.Errorf("seed=%d recovery: %v", *seed, err)
			}
		})
	}
	for _, component := range []harness.Component{harness.App, harness.Executor} {
		logs := docker(t, host, "logs", f.Endpoint(component).Container)
		if strings.Contains(logs, "panic:") || strings.Contains(logs, "fatal error:") {
			t.Errorf("seed=%d %s runtime panic; inspect fixture logs", *seed, component)
		}
	}

}
