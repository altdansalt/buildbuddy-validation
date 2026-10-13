package envargs_test

import (
	"context"
	"flag"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"math/rand"
	"strings"
	"testing"
	"time"
)

var propertySeed = flag.Int64("envargs.seed", 20261012, "replay randomized environment and argv cases")

func platform(docker bool) *repb.Platform {
	p := &repb.Platform{Properties: []*repb.Platform_Property{{Name: "Arch", Value: "amd64"}, {Name: "OSFamily", Value: "linux"}}}
	if docker {
		p.Properties = append(p.Properties, &repb.Platform_Property{Name: "container-image", Value: "docker://alpine:3.21.3"}, &repb.Platform_Property{Name: "workload-isolation-type", Value: "docker"})
	} else {
		p.Properties = append(p.Properties, &repb.Platform_Property{Name: "workload-isolation-type", Value: "none"})
	}
	return p
}

func TestRound2(t *testing.T) {
	for _, docker := range []bool{false, true} {
		t.Run(fmt.Sprintf("docker=%t", docker), func(t *testing.T) {
			var c *harness.Clients
			if docker {
				c = dockerFixture(t).Clients(harness.App)
			} else {
				c = fixture(t)
			}
			root := upload(t, c, &repb.Directory{})
			run := func(t *testing.T, cmd *repb.Command) *repb.ExecuteResponse {
				cmd.Platform = platform(docker)
				return execute(t, c, root, cmd)
			}
			t.Run("seeded-fidelity", func(t *testing.T) {
				t.Logf("replay with -envargs.seed=%d", *propertySeed)
				rng := rand.New(rand.NewSource(*propertySeed))
				value := func() string {
					choices := []string{"", "=", "雪λ🙂", "a b\n\t", "$'\"\\", strings.Repeat("long=雪", 4096)}
					return choices[rng.Intn(len(choices))] + strings.Repeat("x", rng.Intn(80))
				}
				for i := 0; i < 24; i++ {
					t.Run(fmt.Sprintf("case-%02d", i), func(t *testing.T) {
						args := []string{"", "雪λ🙂", "a=b=c"}
						for j := 0; j < 1+rng.Intn(20); j++ {
							args = append(args, value())
						}
						env := []*repb.Command_EnvironmentVariable{}
						script := `printf '%s\000' "$@"`
						expected := strings.Join(args, "\x00") + "\x00"
						for j := 0; j < 1+rng.Intn(12); j++ {
							name := fmt.Sprintf("EA_%02d", j)
							v := value()
							if j == 0 {
								v = ""
							}
							env = append(env, &repb.Command_EnvironmentVariable{Name: name, Value: v})
							script += fmt.Sprintf(`; printf '%%s\000' "$%s"`, name)
							expected += v + "\x00"
						}
						cmd := &repb.Command{Arguments: append([]string{"/bin/sh", "-c", script, "argv0"}, args...), EnvironmentVariables: env}
						require.Equal(t, expected, stdout(t, c, run(t, cmd)), "seed=%d case=%d", *propertySeed, i)
					})
				}
			})
			t.Run("execve-limits", func(t *testing.T) {
				// Linux amd64 MAX_ARG_STRLEN includes the terminating NUL (32 pages).
				for _, n := range []int{131000, 131072} {
					for _, env := range []bool{false, true} {
						t.Run(fmt.Sprintf("bytes=%d/env=%t", n, env), func(t *testing.T) {
							v := strings.Repeat("x", n)
							cmd := &repb.Command{Arguments: []string{"/bin/sh", "-c", `printf '%s' "$1"`, "argv0", v}}
							if env {
								cmd.Arguments = []string{"/bin/sh", "-c", `printf '%s' "$BIG"`}
								cmd.EnvironmentVariables = []*repb.Command_EnvironmentVariable{{Name: "BIG", Value: v}}
							}
							start := time.Now()
							r := run(t, cmd)
							if n < 131072 {
								require.Equal(t, v, stdout(t, c, r))
							} else {
								require.Less(t, time.Since(start), 60*time.Second, "must terminate promptly")
								require.NotEqual(t, int32(4), r.GetStatus().GetCode(), "must not time out")
								require.True(t, r.GetStatus().GetCode() != 0 || r.GetResult().GetExitCode() != 0, "oversized exec must fail")
								require.Contains(t, diagnostic(t, c, r), "argument list too long")
							}
						})
					}
				}
				// Aggregate argv exceeds the usual 2 MiB ARG_MAX without oversized strings.
				cmd := &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf unexpected"}}
				for j := 0; j < 40; j++ {
					cmd.Arguments = append(cmd.Arguments, strings.Repeat("x", 60000))
				}
				start := time.Now()
				r := run(t, cmd)
				require.Less(t, time.Since(start), 60*time.Second)
				require.NotEqual(t, int32(4), r.GetStatus().GetCode())
				require.Contains(t, diagnostic(t, c, r), "argument list too long")
				require.Equal(t, "healthy", stdout(t, c, run(t, &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf healthy"}})))
			})
			t.Run("qualified-and-system-path", func(t *testing.T) {
				root = scriptTree(t, c)
				for _, executable := range []string{"./tools/envargs-tool", "tools/envargs-tool"} {
					require.Equal(t, "input-tool:ok\n", stdout(t, c, run(t, &repb.Command{Arguments: []string{executable, "ok"}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "PATH", Value: "/nonexistent"}}})))
				}
				require.Equal(t, "ok\n", stdout(t, c, run(t, &repb.Command{Arguments: []string{"sh", "-c", "printf 'ok\\n'"}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "PATH", Value: "/bin:/usr/bin"}}})))
				if docker {
					// Resolve an absolute input-tree PATH using the command's actual workspace.
					wd := strings.TrimSuffix(stdout(t, c, run(t, &repb.Command{Arguments: []string{"/bin/sh", "-c", "pwd"}})), "\n")
					require.True(t, strings.HasPrefix(wd, "/"))
					require.Equal(t, "input-tool:absolute-path\n", stdout(t, c, run(t, &repb.Command{Arguments: []string{"envargs-tool", "absolute-path"}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "PATH", Value: "/nonexistent:" + wd + "/tools"}}})))
					for _, path := range []string{"tools", "missing:tools", ":tools"} {
						r := run(t, &repb.Command{Arguments: []string{"envargs-tool", "ok"}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "PATH", Value: path}}})
						// Hosted Docker's runc refuses relative PATH lookup (Go ErrDot).
						// Preserve its clean rejection as runtime-specific coverage.
						require.NotZero(t, r.GetStatus().GetCode())
						require.Contains(t, r.GetStatus().GetMessage(), "cannot run executable found relative to current directory")
					}
				}
			})
		})
	}
}

// Docker reports execve errors through action stderr, while bare execution
// reports a non-OK execution status. Both must retain a useful diagnostic.
func diagnostic(t *testing.T, c *harness.Clients, r *repb.ExecuteResponse) string {
	t.Helper()
	b := r.GetResult().GetStderrRaw()
	if d := r.GetResult().GetStderrDigest(); d != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var err error
		b, err = c.Read(ctx, instance, d)
		require.NoError(t, err)
	}
	text := strings.ToLower(r.GetStatus().GetMessage() + string(b))
	t.Logf("failure diagnostic: %s", text)
	return text
}
