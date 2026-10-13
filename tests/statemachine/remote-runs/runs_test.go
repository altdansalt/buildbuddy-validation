package remoteruns_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Wire field numbers are from the released v2.314.0 proto/{runner,git,
// invocation,execution_stats,eventlog,api/v1/*}.proto; no source is built.
func join(p ...[]byte) []byte { return bytes.Join(p, nil) }
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func str(n protowire.Number, s string) []byte { return field(n, []byte(s)) }
func num(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}

type wire struct {
	b []byte
	v uint64
}

func fields(b []byte, n protowire.Number) []wire {
	var out []wire
	for len(b) > 0 {
		tag, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("invalid response tag")
		}
		b = b[k:]
		var w wire
		switch typ {
		case protowire.BytesType:
			w.b, k = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			w.v, k = protowire.ConsumeVarint(b)
		default:
			k = protowire.ConsumeFieldValue(tag, typ, b)
		}
		if k < 0 {
			panic("invalid response field")
		}
		b = b[k:]
		if tag == n {
			out = append(out, w)
		}
	}
	return out
}
func first(b []byte, n protowire.Number) wire {
	a := fields(b, n)
	if len(a) == 0 {
		return wire{}
	}
	return a[0]
}

type codec struct{}

func (codec) Name() string                    { return "proto" }
func (codec) Marshal(v any) ([]byte, error)   { return v.([]byte), nil }
func (codec) Unmarshal(b []byte, v any) error { *(v.(*[]byte)) = bytes.Clone(b); return nil }
func call(ctx context.Context, c *harness.Clients, service, method string, b []byte) ([]byte, error) {
	var out []byte
	err := c.Conn.Invoke(ctx, "/"+service+"/"+method, b, &out, grpc.ForceCodec(codec{}))
	return out, err
}

var checkLiveCLI = flag.Bool("remote-runs.check-live-cli", false, "Check bb view against a held running invocation")

const ui = "buildbuddy.service.BuildBuddyService"
const apiService = "api.v1.ApiService"

func rest(t *testing.T, f *harness.Fixture, method string, req map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	r, err := http.NewRequest("POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(b))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-buildbuddy-api-key", f.APIKey)
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(r)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, string(b))
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}
func objects(m map[string]any, k string) []any         { a, _ := m[k].([]any); return a }
func object(m map[string]any, k string) map[string]any { a, _ := m[k].(map[string]any); return a }
func boolean(m map[string]any, k string) bool          { b, _ := m[k].(bool); return b }

type model struct {
	id, marker, commit string
	exit               uint64
}

func TestRemoteRunSurfaces(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.Chmod(repo, 0755))
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "fixture@example.invalid"}, {"config", "user.name", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		b, err := cmd.CombinedOutput()
		require.NoError(t, err, string(b))
	}
	require.NoError(t, os.WriteFile(filepath.Join(repo, "fixture.txt"), []byte("fixture-local repository\n"), 0644))
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		b, err := cmd.CombinedOutput()
		require.NoError(t, err, string(b))
	}
	gitHead := exec.Command("git", "rev-parse", "HEAD")
	gitHead.Dir = repo
	head, err := gitHead.Output()
	require.NoError(t, err)
	gitConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(gitConfig, []byte("[safe]\n\tdirectory = /fixture-repo/.git\n"), 0644))
	control := t.TempDir()
	require.NoError(t, os.Chmod(control, 0755))
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy},
		DockerArgs: map[harness.Component][]string{harness.Executor: {"--volume", repo + ":/fixture-repo:ro", "--volume", control + ":/fixture-control", "--volume", gitConfig + ":/etc/gitconfig:ro"}},
		Config:     map[harness.Component]harness.Config{harness.App: {"remote_execution": harness.Config{"write_execution_progress_state_to_redis": true}, "app": harness.Config{"events_api_url": "grpc://app:1985", "cache_api_url": "grpc://app:1985", "remote_execution_api_url": "grpc://app:1985"}}}})
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	ids := map[string]bool{}
	// Repeat valid Run operations: Run is a creation operation, not an
	// idempotent upsert. Every response must identify an independent run.
	for _, surface := range []string{"ui-grpc", "ui-http", "api-grpc", "api-rest"} {
		t.Run(surface, func(t *testing.T) {
			for repeat := 0; repeat < 2; repeat++ {
				m := model{commit: strings.TrimSpace(string(head)), marker: "conformance-" + surface, exit: uint64(repeat * 7)}
				step := fmt.Sprintf("cat fixture.txt; printf '%s\\n'; printf '%s\\n' > \"$BUILDBUDDY_ARTIFACTS_DIRECTORY/result.txt\"; exit %d", m.marker, m.marker, m.exit)
				env := map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": "/fixture-repo/.git"}
				props := map[string]string{"OSFamily": "linux", "Arch": "amd64", "workload-isolation-type": "none"}
				wait := uint64(3)
				if repeat == 0 {
					wait = 1
					if strings.HasPrefix(surface, "api-") {
						wait = 2
					}
				}
				var gate *os.File
				if repeat == 0 {
					gateName := surface + ".fifo"
					gatePath := filepath.Join(control, gateName)
					require.NoError(t, syscall.Mkfifo(gatePath, 0666))
					require.NoError(t, os.Chmod(gatePath, 0666))
					var err error
					gate, err = os.OpenFile(gatePath, os.O_RDWR, 0)
					require.NoError(t, err)
					defer gate.Close()
					defer gate.WriteString("release\n")
					step = "printf '" + m.marker + "-started\\n'; read release < /fixture-control/" + gateName + "; " + step
				}
				if surface == "ui-grpc" || surface == "api-grpc" {
					var p []byte
					for _, k := range []string{"OSFamily", "Arch", "workload-isolation-type"} {
						entry := join(str(1, k), str(2, props[k]))
						if surface == "ui-grpc" {
							p = append(p, field(13, entry)...)
						} else {
							p = append(p, field(6, entry)...)
						}
					}
					envField := protowire.Number(11)
					if surface == "api-grpc" {
						envField = 5
					}
					for k, v := range env {
						p = append(p, field(envField, join(str(1, k), str(2, v)))...)
					}
					service, b, idField := ui, join(field(2, str(1, "file:///fixture-repo")), field(3, str(3, "main")), field(16, str(1, step)), num(20, wait), str(12, "2m"), p), protowire.Number(2)
					if surface == "api-grpc" {
						service = apiService
						idField = 1
						b = join(str(1, "file:///fixture-repo"), str(2, "main"), field(4, str(1, step)), num(13, wait), str(7, "2m"), p)
					}
					out, err := call(ctx, app, service, "Run", b)
					require.NoError(t, err)
					m.id = string(first(out, idField).b)
				} else {
					req := map[string]any{"repo": "file:///fixture-repo", "branch": "main", "steps": []any{map[string]any{"run": step}}, "platformProperties": props, "env": env, "timeout": "2m", "waitUntil": wait}
					var out map[string]any
					if surface == "ui-http" {
						a := []any{}
						for k, v := range props {
							a = append(a, map[string]any{"name": k, "value": v})
						}
						req = map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "gitRepo": map[string]any{"repoUrl": "file:///fixture-repo"}, "repoState": map[string]any{"branch": "main"}, "steps": req["steps"], "execProperties": a, "env": env, "timeout": "2m", "waitUntil": wait}
						var err error
						out, err = f.Web.RPC("Run", req)
						require.NoError(t, err)
					} else {
						out = rest(t, f, "Run", req)
					}
					m.id, _ = out["invocationId"].(string)
				}
				require.NotEmpty(t, m.id)
				require.False(t, ids[m.id], "repeated Run reused identity")
				ids[m.id] = true
				if gate != nil {
					checkRunning(t, ctx, f, app, m)
					_, err := gate.WriteString("release\n")
					require.NoError(t, err)
				}
				// Bounded condition polling tolerates publication lag without assuming a
				// fixed duration or requiring the scheduler to expose every fleeting stage.
				var completed []byte
				deadline := time.NewTimer(2 * time.Minute)
				ticker := time.NewTicker(100 * time.Millisecond)
				for completed == nil {
					out, err := call(ctx, app, ui, "GetInvocation", field(2, str(1, m.id)))
					if err == nil {
						inv := first(out, 2).b
						ex, exErr := call(ctx, app, ui, "GetExecution", join(field(2, str(1, m.id)), num(3, 1)))
						if first(inv, 10).v == 1 && exErr == nil && first(first(ex, 2).b, 2).v == 4 && len(first(first(ex, 2).b, 13).b) > 0 {
							completed = inv
							break
						}
					}
					select {
					case <-deadline.C:
						t.Fatalf("run %s did not complete: %v", m.id, err)
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				deadline.Stop()
				ticker.Stop()
				check(t, ctx, f, app, proxy, m, completed)
			}
		})
	}
}

func check(t *testing.T, ctx context.Context, f *harness.Fixture, app, proxy *harness.Clients, m model, inv []byte) {
	t.Helper()
	require.Equal(t, m.id, string(first(inv, 1).b))
	require.Equal(t, m.commit, string(first(inv, 16).b))
	require.Equal(t, "main", string(first(inv, 23).b))
	debugLog, debugErr := call(ctx, app, ui, "GetEventLogChunk", join(str(2, m.id), num(4, 100000)))
	require.NoError(t, debugErr)
	require.Equal(t, m.exit == 0, first(inv, 3).v != 0, string(first(debugLog, 4).b))
	u, err := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": m.id}})
	require.NoError(t, err)
	require.Len(t, objects(u, "invocation"), 1)
	uiInv := objects(u, "invocation")[0].(map[string]any)
	require.Equal(t, m.id, uiInv["invocationId"])
	require.Equal(t, "COMPLETE_INVOCATION_STATUS", uiInv["invocationStatus"])
	require.Equal(t, m.exit == 0, boolean(uiInv, "success"))
	selector := field(1, str(1, m.id))
	a, err := call(ctx, app, apiService, "GetInvocation", join(selector, num(4, 1)))
	require.NoError(t, err)
	require.Len(t, fields(a, 1), 1)
	publicInv := first(a, 1).b
	require.Equal(t, m.id, string(first(first(publicInv, 1).b, 1).b))
	require.Equal(t, m.exit == 0, first(publicInv, 3).v != 0)
	require.Equal(t, uint64(1), first(publicInv, 25).v)
	require.Equal(t, m.commit, string(first(publicInv, 16).b))
	require.Equal(t, "main", string(first(publicInv, 20).b))
	require.Equal(t, string(first(inv, 15).b), string(first(publicInv, 15).b))
	r := rest(t, f, "GetInvocation", map[string]any{"selector": map[string]any{"invocationId": m.id}, "includeArtifacts": true})
	require.Len(t, objects(r, "invocation"), 1)
	restInv := objects(r, "invocation")[0].(map[string]any)
	require.Equal(t, m.id, object(restInv, "id")["invocationId"])
	require.Equal(t, m.exit == 0, boolean(restInv, "success"))
	require.Equal(t, "COMPLETE_INVOCATION_STATUS", restInv["invocationStatus"])
	require.Equal(t, m.commit, uiInv["commitSha"])
	require.Equal(t, m.commit, restInv["commitSha"])
	require.Equal(t, "main", uiInv["branchName"])
	require.Equal(t, "main", restInv["branchName"])
	require.Equal(t, uiInv["repoUrl"], restInv["repoUrl"])
	require.Equal(t, string(first(inv, 26).b), string(first(publicInv, 23).b))
	require.Equal(t, uiInv["bazelExitCode"], restInv["bazelExitCode"])
	log, err := call(ctx, app, ui, "GetEventLogChunk", join(str(2, m.id), num(4, 100000)))
	require.NoError(t, err)
	text := string(first(log, 4).b)
	require.Contains(t, text, m.marker)
	require.Contains(t, text, "fixture-local repository")
	u, err = f.Web.RPC("GetEventLogChunk", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": m.id, "minLines": 100000})
	require.NoError(t, err)
	decoded, err := base64.StdEncoding.DecodeString(u["buffer"].(string))
	require.NoError(t, err)
	require.Equal(t, text, string(decoded))
	a, err = call(ctx, app, apiService, "GetLog", selector)
	require.NoError(t, err)
	require.Equal(t, text, string(first(first(a, 1).b, 3).b))
	r = rest(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": m.id}})
	require.Equal(t, text, object(r, "log")["contents"])
	execs, err := call(ctx, app, ui, "GetExecution", join(field(2, str(1, m.id)), num(3, 1)))
	require.NoError(t, err)
	require.Len(t, fields(execs, 2), 1)
	e := first(execs, 2).b
	require.NotEmpty(t, string(first(e, 14).b))
	require.Equal(t, uint64(4), first(e, 2).v)
	require.Equal(t, uint64(0), first(first(e, 3).b, 1).v)
	require.Equal(t, m.exit, first(e, 8).v)
	u, err = f.Web.RPC("GetExecution", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "executionLookup": map[string]any{"invocationId": m.id}, "inlineExecuteResponse": true})
	require.NoError(t, err)
	require.Len(t, objects(u, "execution"), 1)
	ue := objects(u, "execution")[0].(map[string]any)
	require.Equal(t, string(first(e, 14).b), ue["executionId"])
	require.Equal(t, "COMPLETED", ue["stage"])
	exit, _ := ue["exitCode"].(float64)
	require.Equal(t, float64(m.exit), exit)
	// ExecuteResponse CAS is shared by app and proxy, although the proxy does
	// not implement Run or the invocation/UI service.
	var er repb.ExecuteResponse
	require.NoError(t, proto.Unmarshal(first(e, 13).b, &er))
	require.NotNil(t, er.Result)
	require.Equal(t, int32(m.exit), er.Result.ExitCode)
	// Artifacts are published as invocation attachments and are readable through
	// the public API. The exact upload URI also gives us a cross-cache oracle.
	require.NotEmpty(t, objects(restInv, "artifacts"))
	var grpcArtifacts, restArtifacts []string
	for _, a := range fields(publicInv, 24) {
		grpcArtifacts = append(grpcArtifacts, string(first(a.b, 1).b)+"|"+string(first(a.b, 2).b))
	}
	for _, a := range objects(restInv, "artifacts") {
		a := a.(map[string]any)
		restArtifacts = append(restArtifacts, fmt.Sprint(a["name"])+"|"+fmt.Sprint(a["uri"]))
	}
	require.ElementsMatch(t, grpcArtifacts, restArtifacts)
	for _, v := range objects(restInv, "artifacts") {
		art := v.(map[string]any)
		uri, _ := art["uri"].(string)
		require.True(t, strings.HasPrefix(uri, "bytestream://"), uri)
		resource := strings.TrimPrefix(uri, "bytestream://")
		require.True(t, strings.Contains(resource, "/"), uri)
		resource = resource[strings.Index(resource, "/")+1:]
		for _, c := range []*harness.Clients{app, proxy} {
			stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: resource})
			require.NoError(t, err)
			var contents []byte
			for {
				chunk, err := stream.Recv()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				contents = append(contents, chunk.Data...)
			}
			require.Equal(t, m.marker+"\n", string(contents), uri)
		}
	}
	a, err = call(ctx, app, apiService, "GetAction", selector)
	require.NoError(t, err)
	r = rest(t, f, "GetAction", map[string]any{"selector": map[string]any{"invocationId": m.id}})
	require.Equal(t, len(fields(a, 1)), len(objects(r, "action")))
	var wireActions, restActions []string
	for _, w := range fields(a, 1) {
		id := first(w.b, 1).b
		require.Equal(t, m.id, string(first(id, 1).b))
		wireActions = append(wireActions, fmt.Sprintf("%s|%s|%s|%s", first(id, 1).b, first(id, 2).b, first(id, 3).b, first(id, 4).b))
	}
	for _, v := range objects(r, "action") {
		id := object(v.(map[string]any), "id")
		get := func(k string) string { v, _ := id[k].(string); return v }
		require.Equal(t, m.id, get("invocationId"))
		restActions = append(restActions, fmt.Sprintf("%s|%s|%s|%s", get("invocationId"), get("targetId"), get("configurationId"), get("actionId")))
	}
	require.ElementsMatch(t, wireActions, restActions)
	checkCLI(t, ctx, f, app, m, e)
}

// Obtain the released CLI embedded in the Run input root, rather than depending
// on a runner installation or downloading/building an unrelated CLI version.
func releasedCLI(t *testing.T, ctx context.Context, app *harness.Clients, e []byte) string {
	t.Helper()
	var digest repb.Digest
	require.NoError(t, proto.Unmarshal(first(e, 1).b, &digest))
	readProto := func(d *repb.Digest, m proto.Message) {
		r, err := app.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "bb-snapshot", Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_BLAKE3})
		require.NoError(t, err)
		require.Len(t, r.Responses, 1)
		require.Equal(t, int32(0), r.Responses[0].Status.GetCode())
		require.NoError(t, proto.Unmarshal(r.Responses[0].Data, m))
	}
	var action repb.Action
	readProto(&digest, &action)
	var root repb.Directory
	readProto(action.InputRootDigest, &root)
	for _, file := range root.Files {
		if file.Name != "bb" && file.Name != "bb.exe" {
			continue
		}
		stream, err := app.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: fmt.Sprintf("bb-snapshot/blobs/blake3/%s/%d", file.Digest.Hash, file.Digest.SizeBytes)})
		require.NoError(t, err)
		var b []byte
		for {
			r, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			b = append(b, r.Data...)
		}
		path := filepath.Join(t.TempDir(), "bb")
		require.NoError(t, os.WriteFile(path, b, 0755))
		return path
	}
	t.Fatal("released Run input root has no embedded CLI")
	return ""
}

func checkCLI(t *testing.T, ctx context.Context, f *harness.Fixture, app *harness.Clients, m model, e []byte) {
	t.Helper()
	cli := releasedCLI(t, ctx, app, e)
	home := t.TempDir()
	run := func(args ...string) []byte {
		cmd := exec.CommandContext(ctx, cli, args...)
		cmd.Dir = home
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "BUILDBUDDY_API_KEY=" + f.APIKey}
		b, err := cmd.CombinedOutput()
		require.NoError(t, err, string(b))
		return b
	}
	b := run("view", m.id, "--target=grpc://"+f.Endpoint(harness.App).GRPC)
	require.Contains(t, string(b), m.marker)
	dest := t.TempDir()
	run("download", "artifacts", m.id, "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--output_directory="+dest)
	found := false
	require.NoError(t, filepath.WalkDir(dest, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		require.Equal(t, m.marker+"\n", string(b))
		found = true
		return nil
	}))
	require.True(t, found, "bb download produced no artifacts")
}

func checkRunning(t *testing.T, ctx context.Context, f *harness.Fixture, app *harness.Clients, m model) {
	t.Helper()
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var inv, execution []byte
	chunkID := ""
	for {
		i, ie := call(ctx, app, ui, "GetInvocation", field(2, str(1, m.id)))
		e, ee := call(ctx, app, ui, "GetExecution", field(2, str(1, m.id)))
		l, le := call(ctx, app, ui, "GetEventLogChunk", join(str(2, m.id), str(3, chunkID), num(4, 100000)))
		if next := string(first(l, 3).b); next != "" {
			chunkID = next
		}
		if ie == nil && ee == nil && le == nil && first(first(i, 2).b, 10).v == 2 && first(first(e, 2).b, 2).v == 3 && strings.Contains(string(first(l, 4).b), m.marker+"-started") {
			inv = first(i, 2).b
			execution = first(e, 2).b
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("run %s never converged to running: invocation=%d execution=%d errors=%v/%v/%v log=%s", m.id, first(first(i, 2).b, 10).v, first(first(e, 2).b, 2).v, ie, ee, le, string(first(l, 4).b))
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	require.Equal(t, m.id, string(first(inv, 1).b))
	u, err := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": m.id}})
	require.NoError(t, err)
	require.Len(t, objects(u, "invocation"), 1)
	require.Equal(t, "PARTIAL_INVOCATION_STATUS", objects(u, "invocation")[0].(map[string]any)["invocationStatus"])
	a, err := call(ctx, app, apiService, "GetInvocation", field(1, str(1, m.id)))
	require.NoError(t, err)
	require.Equal(t, uint64(2), first(first(a, 1).b, 25).v)
	r := rest(t, f, "GetInvocation", map[string]any{"selector": map[string]any{"invocationId": m.id}})
	require.Len(t, objects(r, "invocation"), 1)
	require.Equal(t, "PARTIAL_INVOCATION_STATUS", objects(r, "invocation")[0].(map[string]any)["invocationStatus"])
	liveLog, liveErr := f.Web.RPC("GetEventLogChunk", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "invocationId": m.id, "chunkId": chunkID, "minLines": 100000})
	require.NoError(t, liveErr)
	liveBytes, liveErr := base64.StdEncoding.DecodeString(liveLog["buffer"].(string))
	require.NoError(t, liveErr)
	require.Contains(t, string(liveBytes), m.marker+"-started")
	a, err = call(ctx, app, apiService, "GetLog", join(field(1, str(1, m.id)), str(3, chunkID)))
	require.NoError(t, err)
	require.Contains(t, string(first(first(a, 1).b, 3).b), m.marker+"-started")
	logs := rest(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": m.id}, "pageToken": chunkID})
	require.Contains(t, object(logs, "log")["contents"], m.marker+"-started")
	u, err = f.Web.RPC("GetExecution", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "executionLookup": map[string]any{"invocationId": m.id}})
	require.NoError(t, err)
	require.Len(t, objects(u, "execution"), 1)
	require.Equal(t, string(first(execution, 14).b), objects(u, "execution")[0].(map[string]any)["executionId"])
	require.Equal(t, "EXECUTING", objects(u, "execution")[0].(map[string]any)["stage"])
	if *checkLiveCLI {
		liveCLI(t, ctx, f, app, m, execution)
	}
}

func liveCLI(t *testing.T, ctx context.Context, f *harness.Fixture, app *harness.Clients, m model, e []byte) {
	t.Helper()
	cli := releasedCLI(t, ctx, app, e)
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, cli, "view", m.id, "--target=grpc://"+f.Endpoint(harness.App).GRPC)
	cmd.Dir = home
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "BUILDBUDDY_API_KEY=" + f.APIKey}
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, string(b))
	require.Contains(t, string(b), m.marker+"-started", "API live logs contain this marker while the fixture-local shell waits at its FIFO")
}
