package platformprops_test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

func runAction(t *testing.T, c *harness.Clients, p *repb.Platform, commandPlatform bool, script string, cache bool) *repb.ExecuteResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const instance = "platform-props-round2"
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	command := &repb.Command{Arguments: []string{"/bin/sh", "-c", script}}
	action := &repb.Action{InputRootDigest: root, Timeout: durationpb.New(20 * time.Second), DoNotCache: !cache}
	if commandPlatform {
		command.Platform = p
	} else {
		action.Platform = p
	}
	action.CommandDigest, err = c.UploadProto(ctx, instance, command)
	require.NoError(t, err)
	d, err := c.UploadProto(ctx, instance, action)
	require.NoError(t, err)
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: !cache})
	require.NoError(t, err)
	for {
		op, err := stream.Recv()
		require.NotEqual(t, io.EOF, err)
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		r := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(r))
		if r.GetResult().GetStdoutDigest() != nil {
			r.Result.StdoutRaw, err = c.Read(ctx, instance, r.Result.StdoutDigest)
			require.NoError(t, err)
		}
		return r
	}
}
func successful(t *testing.T, r *repb.ExecuteResponse, want string) {
	t.Helper()
	require.Zero(t, r.GetStatus().GetCode(), r.GetStatus().GetMessage())
	require.NotNil(t, r.Result)
	require.Zero(t, r.Result.ExitCode)
	require.Equal(t, want, string(r.Result.StdoutRaw))
}

func TestResourceEstimatesRound2(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	// Malformed estimates deliberately fall back to defaults in the released parser.
	for _, name := range []string{"EstimatedCPU", "EstimatedMemory", "EstimatedFreeDiskBytes"} {
		for _, value := range []string{"", "0", "-1", "0.125", "1e3", "NaN", "+Inf", "-Inf", "nonsense", "250m", "1KB", "1KiB", " 0 "} {
			t.Run(name+"/"+value, func(t *testing.T) {
				code, msg := execute(t, f.Clients(harness.App), base(name, value), nil)
				t.Logf("%s=%q: %s %s", name, value, code, msg)
				if name == "EstimatedCPU" && value == "1e3" {
					require.Equal(t, codes.Unavailable, code, msg)
					require.Contains(t, msg, "milli_cpu=1000000")
				} else {
					require.Equal(t, codes.OK, code, msg)
				}
			})
		}
	}
}

func TestCacheAndConcurrentPropertyIsolation(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	// Fixed seeded permutation is replayable. The command is identical across all
	// values: only one behavior-changing platform property differs.
	const seed int64 = 20261012
	t.Logf("seed=%d", seed)
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("command_platform_%t", fallback), func(t *testing.T) {
			for _, i := range rand.New(rand.NewSource(seed)).Perm(6) {
				value := fmt.Sprintf("value%d", i)
				p := base("env-overrides", "ROUND2="+value)
				script := `printf '%s' "$ROUND2"`
				if fallback {
					script += " # command fallback"
				}
				r := runAction(t, c, p, fallback, script, true)
				successful(t, r, value)
				require.False(t, r.CachedResult, "different properties must execute independently")
				r = runAction(t, c, p, fallback, script, true)
				successful(t, r, value)
				require.True(t, r.CachedResult, "control must hit action cache")
			}
		})
	}
	t.Run("concurrent", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			i := i
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				value := fmt.Sprintf("concurrent%d", i)
				successful(t, runAction(t, c, base("env-overrides", "ROUND2="+value, "EstimatedCPU", "125m", "EstimatedMemory", "8MiB"), false, `sleep 0.1; printf '%s' "$ROUND2"`, false), value)
			})
		}
	})
}

func TestDockerImageReferencesRound2(t *testing.T) {
	f := dockerFixture(t)
	c := f.Clients(harness.App)
	imagePlatform := func(ref string) *repb.Platform {
		return platform("OSFamily", "linux", "Arch", "amd64", "workload-isolation-type", "docker", "container-image", "docker://"+ref)
	}
	successful(t, runAction(t, c, imagePlatform("alpine:3.21.3"), false, "cat /etc/alpine-release", false), "3.21.3\n")
	b, err := exec.Command("docker", "image", "inspect", "alpine:3.21.3", "--format", "{{index .RepoDigests 0}}").CombinedOutput()
	require.NoError(t, err, string(b))
	digest := strings.Split(strings.TrimSpace(string(b)), "@")[1]
	for _, ref := range []string{"alpine@" + digest, "alpine:3.21.3@" + digest, "alpine:nonexistent-tag@" + digest} {
		t.Run(ref, func(t *testing.T) {
			successful(t, runAction(t, c, imagePlatform(ref), false, "cat /etc/alpine-release", false), "3.21.3\n")
		})
	}
	t.Run("cache_image_property", func(t *testing.T) {
		for _, ref := range []string{"alpine:3.21.3", "alpine@" + digest} {
			p := imagePlatform(ref)
			r := runAction(t, c, p, false, "cat /etc/alpine-release # cache image", true)
			successful(t, r, "3.21.3\n")
			require.False(t, r.CachedResult)
			r = runAction(t, c, p, false, "cat /etc/alpine-release # cache image", true)
			successful(t, r, "3.21.3\n")
			require.True(t, r.CachedResult)
		}
	})
	t.Run("cache_docker_user_property", func(t *testing.T) {
		for _, uid := range []string{"12345", "23456"} {
			p := imagePlatform("alpine:3.21.3")
			p.Properties = append(p.Properties, &repb.Platform_Property{Name: "dockerUser", Value: uid})
			sort.Slice(p.Properties, func(i, j int) bool { return p.Properties[i].Name < p.Properties[j].Name })
			r := runAction(t, c, p, false, "id -u", true)
			successful(t, r, uid+"\n")
			require.False(t, r.CachedResult)
			r = runAction(t, c, p, false, "id -u", true)
			successful(t, r, uid+"\n")
			require.True(t, r.CachedResult)
		}
	})
	for _, ref := range []string{"Uppercase/repo:tag", "alpine@@sha256:bad", "alpine@sha256:bad", ":bad", "alpine:", "alpine:tag:extra", "alpine with spaces"} {
		t.Run(ref, func(t *testing.T) {
			r := runAction(t, c, imagePlatform(ref), false, "echo must-not-run", false)
			t.Logf("status=%v", r.GetStatus())
			require.NotZero(t, r.GetStatus().GetCode())
			require.Empty(t, r.GetResult().GetStdoutRaw())
		})
	}
	t.Run("registry_port", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		ref := strings.TrimPrefix(server.URL, "http://") + "/repo:tag"
		r := runAction(t, c, imagePlatform(ref), false, "echo must-not-run", false)
		require.Positive(t, requests.Load(), "registry with port must be parsed and contacted")
		require.NotZero(t, r.GetStatus().GetCode())
		require.Empty(t, r.GetResult().GetStdoutRaw())
	})
	successful(t, runAction(t, c, imagePlatform("alpine:3.21.3"), false, "cat /etc/alpine-release", false), "3.21.3\n")
}
