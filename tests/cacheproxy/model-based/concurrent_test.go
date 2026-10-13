package model_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type concurrentOp struct {
	Worker, Endpoint int
	Write            bool
	Value            int32
}
type round struct {
	Restart string
	Ops     []concurrentOp
}
type observation struct {
	Op         concurrentOp
	Start, End int64
	Value      int32
	Miss       bool
	Err        string
}

var replay = flagTrace()

func flagTrace() *string {
	return flag.String("model.trace", "", "JSON round trace to replay (printed on failure)")
}

// Each round is small enough for exhaustive search. Successful reads must see
// a value at a legal linearization point; misses remain legal eviction.
func linearizable(h []observation, initial int32) bool {
	var search func(uint64, int32) bool
	search = func(done uint64, value int32) bool {
		if done == (uint64(1)<<len(h))-1 {
			return true
		}
		for i, o := range h {
			if done&(1<<i) != 0 || o.Err != "" {
				continue
			}
			blocked := false
			for j, p := range h {
				if done&(1<<j) == 0 && p.End < o.Start {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			next := value
			if o.Op.Write {
				if o.Value != o.Op.Value {
					continue
				}
				next = o.Op.Value
			} else if !o.Miss && o.Value != value {
				continue
			}
			if search(done|(1<<i), next) {
				return true
			}
		}
		return false
	}
	return search(0, initial)
}

func TestConcurrentRestarts(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	rng := rand.New(rand.NewSource(*seed))
	trace := make([]round, 12)
	for i := range trace {
		if i%3 == 1 {
			trace[i].Restart = string(harness.App)
		}
		if i%3 == 2 {
			trace[i].Restart = string(harness.CacheProxy)
		}
		for w := 0; w < 4; w++ {
			for j := 0; j < 2; j++ {
				trace[i].Ops = append(trace[i].Ops, concurrentOp{w, rng.Intn(2), rng.Intn(2) == 0, int32(1 + i*8 + w*2 + j)})
			}
		}
	}
	if *replay != "" {
		if err := json.Unmarshal([]byte(*replay), &trace); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range trace {
		if r.Restart != "" && r.Restart != string(harness.App) && r.Restart != string(harness.CacheProxy) {
			t.Fatal("invalid restart component")
		}
		if len(r.Ops) > 12 {
			t.Fatal("replay histories are limited to 12 operations")
		}
		for _, op := range r.Ops {
			if op.Worker < 0 || op.Worker >= 4 || op.Endpoint < 0 || op.Endpoint >= 2 {
				t.Fatal("invalid worker or endpoint")
			}
		}
	}
	runID := 0
	run := func(tr []round) (string, error) {
		runID++
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		instance := fmt.Sprintf("concurrent/%d/%d", *seed, runID)
		c := clients[0]
		cd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"echo", instance}})
		if err != nil {
			return "", err
		}
		root, err := c.UploadProto(ctx, instance, &repb.Directory{})
		if err != nil {
			return "", err
		}
		ad, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cd, InputRootDigest: root})
		if err != nil {
			return "", err
		}
		payload := []byte(instance)
		d, err := clients[1].Upload(ctx, instance, payload)
		if err != nil {
			return "", err
		}
		for ri, r := range tr {
			// Establish a known state before every bounded concurrent history.
			_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: ad, ActionResult: &repb.ActionResult{ExitCode: 0}})
			if err != nil {
				return "", err
			}
			if r.Restart != "" {
				if err := restart(ctx, f, harness.Component(r.Restart)); err != nil {
					return "", err
				}
			}
			if r.Restart != "" {
				clients = []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
				c = clients[0]
			}
			var clock atomic.Int64
			obs := make([]observation, len(r.Ops))
			var wg sync.WaitGroup
			gate := make(chan struct{})
			for worker := 0; worker < 4; worker++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					<-gate
					for i, op := range r.Ops {
						if op.Worker != w {
							continue
						}
						o := observation{Op: op, Start: clock.Add(1)}
						peer := clients[op.Endpoint]
						var got *repb.ActionResult
						var e error
						if op.Write {
							got, e = peer.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: ad, ActionResult: &repb.ActionResult{ExitCode: op.Value}})
						} else {
							got, e = peer.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: ad})
						}
						o.End = clock.Add(1)
						if !op.Write && status.Code(e) == codes.NotFound {
							o.Miss = true
						} else if e != nil {
							o.Err = e.Error()
						} else {
							o.Value = got.ExitCode
						}

						// Shared digest across goroutines: immutable CAS writes
						// may overlap freely and every read must return identical bytes.
						if o.Err == "" {
							if op.Write {
								_, e = peer.Write(ctx, instance, payload, op.Value%2 == 0)
							} else {
								var data []byte
								data, e = peer.Read(ctx, instance, d)
								if e == nil && string(data) != string(payload) {
									e = fmt.Errorf("concurrent CAS byte mismatch")
								}
							}
							if e != nil {
								o.Err = e.Error()
							}
						}
						obs[i] = o
					}
				}(worker)
			}
			close(gate)
			wg.Wait()
			for _, o := range obs {
				if o.Err != "" {
					return "", fmt.Errorf("round %d RPC: %s", ri, o.Err)
				}
			}
			if !linearizable(obs, 0) {
				b, _ := json.Marshal(obs)
				return string(b), nil
			}
			// CAS immutable bytes must survive both kinds of restart and concurrent AC traffic.
			for _, peer := range clients {
				b, e := peer.Read(ctx, instance, d)
				if e != nil {
					return "", e
				}
				if string(b) != string(payload) {
					return "CAS byte mismatch", nil
				}
			}
		}
		return "", nil
	}
	failure, err := run(trace)
	if err != nil {
		t.Fatal(err)
	}
	if failure == "" {
		t.Logf("seed=%d checked %d concurrent histories with backend/proxy restarts", *seed, len(trace))
		return
	}
	t.Logf("initial history: %s", failure)
	// Confirm twice before shrinking. Scheduling is intentionally unconstrained;
	// candidates must fail twice too, avoiding reductions based on a lucky race.
	again, err := run(trace)
	if err != nil {
		t.Fatal(err)
	}
	if again == "" {
		t.Fatal("nonlinearizable history did not reproduce; retain initial history above")
	}
	budget := 40
	fails := func(candidate []round) bool {
		if budget < 2 {
			return false
		}
		budget -= 2
		a, e := run(candidate)
		if e != nil || a == "" {
			return false
		}
		b, e := run(candidate)
		return e == nil && b != ""
	}
	trace = shrinkTrace(trace, fails)
	b, _ := json.Marshal(trace)
	t.Fatalf("seed=%d confirmed nonlinearizable history; replay --test_arg=-model.trace=%s", *seed, b)
}

// Deletion shrinking reaches a 1-minimal trace if the replay budget permits.
func shrinkTrace(tr []round, fails func([]round) bool) []round {
	for i := 0; i < len(tr); {
		candidate := append(append([]round{}, tr[:i]...), tr[i+1:]...)
		if len(candidate) > 0 && fails(candidate) {
			tr = candidate
			i = 0
		} else {
			i++
		}
	}
	for i := range tr {
		if tr[i].Restart != "" {
			candidate := append([]round{}, tr...)
			candidate[i].Restart = ""
			if fails(candidate) {
				tr = candidate
			}
		}
		for j := 0; j < len(tr[i].Ops); {
			candidate := append([]round{}, tr...)
			candidate[i].Ops = append(append([]concurrentOp{}, tr[i].Ops[:j]...), tr[i].Ops[j+1:]...)
			if fails(candidate) {
				tr = candidate
				j = 0
			} else {
				j++
			}
		}
	}
	return tr
}

// Area-local workaround until lifecycle controls merge. Discover the daemon by
// inspecting only the exact fixture-owned container, never operate by image/name prefix.
func restart(ctx context.Context, f *harness.Fixture, c harness.Component) error {
	ep := f.Endpoint(c)
	sockets, _ := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	hosts := append([]string{""}, sockets...)
	for _, host := range hosts {
		args := []string{}
		if host != "" {
			args = append(args, "--host", "unix://"+host)
		}
		inspect := exec.CommandContext(ctx, "docker", append(args, "inspect", ep.Container)...)
		if inspect.Run() != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, "docker", append(args, "restart", "--time", "40", ep.Container)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("restart: %w %s", err, out)
		}
		port := func(containerPort string) (string, error) {
			out, e := exec.CommandContext(ctx, "docker", append(args, "port", ep.Container, containerPort)...).Output()
			if e != nil {
				return "", e
			}
			_, p, e := net.SplitHostPort(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
			if e != nil {
				return "", e
			}
			return net.JoinHostPort("127.0.0.1", p), nil
		}
		httpAddr, e := port("8080/tcp")
		if e != nil {
			return e
		}
		grpcAddr, e := port("1985/tcp")
		if e != nil {
			return e
		}
		ep.HTTP = "http://" + httpAddr
		ep.GRPC = grpcAddr
		client := &http.Client{Timeout: time.Second}
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			resp, e := client.Get(ep.HTTP + "/readyz?server-type=" + ep.ServerType)
			if e == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return nil
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("%s readiness timeout after restart", c)
	}
	return fmt.Errorf("fixture container %s daemon not found", ep.Container)
}

func TestLinearizabilityOracle(t *testing.T) {
	write := func(start, end int64, value int32) observation {
		return observation{Op: concurrentOp{Write: true, Value: value}, Start: start, End: end, Value: value}
	}
	read := func(start, end int64, value int32) observation {
		return observation{Start: start, End: end, Value: value}
	}
	for _, tc := range []struct {
		name string
		h    []observation
		want bool
	}{
		{"overlapping read before write", []observation{write(1, 4, 1), read(2, 3, 0)}, true},
		{"overlapping read after write", []observation{write(1, 4, 1), read(2, 3, 1)}, true},
		{"stale read after completed write", []observation{write(1, 2, 1), read(3, 4, 0)}, false},
		{"impossible value", []observation{write(1, 4, 1), read(2, 3, 2)}, false},
		{"conflicting overlapping reads", []observation{write(1, 8, 1), read(2, 3, 1), read(4, 5, 0)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linearizable(tc.h, 0); got != tc.want {
				t.Fatalf("got %t want %t", got, tc.want)
			}
		})
	}
}

func TestTraceShrinker(t *testing.T) {
	tr := []round{{Ops: []concurrentOp{{Value: 1}, {Value: 2}}}, {Ops: []concurrentOp{{Value: 3}, {Value: 4}}}}
	reduced := shrinkTrace(tr, func(candidate []round) bool {
		for _, r := range candidate {
			for _, op := range r.Ops {
				if op.Value == 3 {
					return true
				}
			}
		}
		return false
	})
	if len(reduced) != 1 || len(reduced[0].Ops) != 1 || reduced[0].Ops[0].Value != 3 {
		t.Fatalf("unexpected reduction: %+v", reduced)
	}
}
