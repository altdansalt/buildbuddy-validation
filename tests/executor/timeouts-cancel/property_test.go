package timeouts_test

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

var treeSeed = flag.Int64("tree_seed", 314003, "Replay seed for process-tree timeout/cancellation properties")

// Every shell and its external commands inherit a unique environment token.
// Scanning environ catches live reparented/session-detached processes without
// treating unreaped zombies (which cannot execute or write) as survivors.
func treeProcesses(t *testing.T, ctx context.Context, container, token string) string {
	t.Helper()
	script := fmt.Sprintf(`for p in /proc/[0-9]*; do test -r "$p/environ" || continue; if tr '\000' '\n' < "$p/environ" 2>/dev/null | grep -qx 'BBVAL_TREE=%s'; then echo "${p##*/}"; fi; done`, token)
	b, err := exec.CommandContext(ctx, "docker", "exec", container, "/bin/sh", "-c", script).Output()
	require.NoError(t, err, string(b))
	return strings.TrimSpace(string(b))
}

func actionContainers(t *testing.T, ctx context.Context) []string {
	t.Helper()
	b, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "ancestor=mirror.gcr.io/library/alpine:3.21.3", "--format", "{{.ID}}").Output()
	require.NoError(t, err)
	return strings.Fields(string(b))
}

func treeScript(rng *rand.Rand, token string) (string, int) {
	nodes := 3 + rng.Intn(5)
	// Parent indices always precede their children, producing an acyclic tree.
	children := make([][]int, nodes)
	ignore := make([]bool, nodes)
	detached := make([]bool, nodes)
	for i := range ignore {
		ignore[i] = rng.Intn(2) == 0
		detached[i] = rng.Intn(2) == 0
	}
	// Ensure every replay includes both a grandchild and a detached TERM ignorer.
	children[0] = append(children[0], 1)
	children[1] = append(children[1], 2)
	for i := 3; i < nodes; i++ {
		parent := rng.Intn(i)
		children[parent] = append(children[parent], i)
	}
	detached[2], ignore[2] = true, true
	var node func(int) string
	node = func(i int) string {
		var b strings.Builder
		if ignore[i] {
			b.WriteString("trap '' TERM; ")
		}
		for _, child := range children[i] {
			prefix := ""
			if detached[child] {
				prefix = "setsid "
			}
			fmt.Fprintf(&b, "%s/bin/sh -c %s </dev/null >/dev/null 2>&1 & ", prefix, shellQuote(node(child)))
		}
		// Partial file writes continue until killed; the completion marker is unreachable.
		fmt.Fprintf(&b, "while :; do printf 'node%d\\n' >> partial.txt; sleep 0.1; done", i)
		return b.String()
	}
	return "export BBVAL_TREE=" + token + "; exec /bin/sh -c " + shellQuote("printf started; "+node(0)+"; printf completed"), nodes
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func TestSeededProcessTreeTermination(t *testing.T) {
	for _, isolation := range []string{"bare", "docker"} {
		t.Run(isolation, func(t *testing.T) {
			// Own the daemon in both modes so independent Docker exec probes use
			// the same socket as the fixture, including when no system daemon runs.
			f := dockerFixture(t)
			var props []*repb.Platform_Property
			if isolation == "docker" {
				props = dockerProps()
			}
			ctx, stop := context.WithTimeout(context.Background(), 6*time.Minute)
			defer stop()
			c := f.Clients(harness.App)
			warm := execute(t, ctx, c, upload(t, ctx, c, "command -v setsid && printf ready", 30*time.Second, props...))
			require.Zero(t, warm.GetStatus().GetCode())
			require.Zero(t, warm.GetResult().GetExitCode())
			rng := rand.New(rand.NewSource(*treeSeed))
			for i := 0; i < 8; i++ {
				t.Run(strconv.Itoa(i), func(t *testing.T) {
					token := fmt.Sprintf("tree-%s-%d", f.ID, i)
					script, nodes := treeScript(rng, token)
					cancelAction := i%2 == 1
					delay := time.Duration(1200+rng.Intn(1300)) * time.Millisecond
					timeout := delay
					actionCtx := ctx
					id := ""
					if cancelAction {
						timeout = 60 * time.Second
						id = uuid.NewString()
						invocation(t, ctx, c, id)
						md, err := proto.Marshal(&repb.RequestMetadata{ToolInvocationId: id})
						require.NoError(t, err)
						actionCtx = metadata.AppendToOutgoingContext(ctx, "build.bazel.remote.execution.v2.requestmetadata-bin", string(md))
					}
					t.Logf("seed=%d case=%d isolation=%s nodes=%d cancel=%t delay=%s script=%s", *treeSeed, i, isolation, nodes, cancelAction, delay, script)
					// Deliberately omit termination-grace-period: the separately bug-tagged
					// graceful_timeout_child_test documents the known parent-exits-first exception.
					s := waitExecuting(t, actionCtx, c, uploadTree(t, ctx, c, script, timeout, props...))
					if cancelAction {
						// EXECUTING precedes process startup. Independently witness live action
						// processes before cancellation so a queued cancellation cannot pass.
						deadline := time.Now().Add(10 * time.Second)
						witnessed := false
						for time.Now().Before(deadline) {
							containers := []string{f.Endpoint(harness.Executor).Container}
							if isolation == "docker" {
								containers = actionContainers(t, ctx)
							}
							for _, container := range containers {
								if treeProcesses(t, ctx, container, token) != "" {
									witnessed = true
								}
							}
							if witnessed {
								break
							}
							time.Sleep(100 * time.Millisecond)
						}
						require.True(t, witnessed, "action never started")
						time.Sleep(delay)
						_, err := f.Web.RPC("CancelExecutions", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": id})
						require.NoError(t, err)
					}
					r := terminal(t, s)
					want := codes.DeadlineExceeded
					if cancelAction {
						want = codes.Canceled
					}
					require.Equal(t, int32(want), r.GetStatus().GetCode(), "partial execution must not report success")
					require.NotContains(t, stdout(t, ctx, c, r), "completed")
					for _, output := range r.GetResult().GetOutputFiles() {
						require.Equal(t, "partial.txt", output.GetPath())
						b, err := c.Read(ctx, instance, output.GetDigest())
						require.NoError(t, err)
						require.NotEmpty(t, b)
						require.NotContains(t, string(b), "completed")
						for node := 0; node < nodes; node++ {
							require.Contains(t, string(b), fmt.Sprintf("node%d\n", node), "generated node must have started writing")
						}
					}
					if !cancelAction {
						require.Contains(t, stdout(t, ctx, c, r), "started")
					}
					// Cancellation publishes its terminal operation before executor lease
					// renewal. Allow the documented async cleanup bound, then inspect again.
					deadline := time.Now().Add(25 * time.Second)
					survivors := ""
					for {
						if isolation == "docker" {
							survivors = strings.Join(actionContainers(t, ctx), ",")
						} else {
							survivors = treeProcesses(t, ctx, f.Endpoint(harness.Executor).Container, token)
						}
						if survivors == "" || time.Now().After(deadline) {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
					require.Empty(t, survivors, "live action descendants after cleanup bound")
					time.Sleep(200 * time.Millisecond)
					if isolation == "bare" {
						require.Empty(t, treeProcesses(t, ctx, f.Endpoint(harness.Executor).Container, token))
					} else {
						require.Empty(t, actionContainers(t, ctx))
					}
				})
			}
		})
	}
}
