package upgradecompat_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Previous non-prerelease GitHub release before v2.314.0; linux/amd64
// digests resolved using the GCR manifest API, never mutable tags.
const oldApp = "gcr.io/flame-public/buildbuddy-app-enterprise@sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6"
const oldExecutor = "gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:8f7c2662f5fcd0ab888113f6d36d31cde8b8ea254434a8177cf6996b01155afe"

// Discover only the daemon owning this fixture; the harness may start a private
// daemon rather than using the runner's default socket.
func fixtureDocker(t *testing.T, f *harness.Fixture) func(...string) string {
	t.Helper()
	sockets, err := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	require.NoError(t, err)
	hosts := append([]string{""}, sockets...)
	for _, host := range hosts {
		prefix := []string{}
		if host != "" {
			prefix = []string{"--host", "unix://" + host}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := exec.CommandContext(ctx, "docker", append(prefix, "inspect", f.Endpoint(harness.App).Container)...).CombinedOutput()
		cancel()
		if err != nil {
			continue
		}
		return func(args ...string) string {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			b, err := exec.CommandContext(ctx, "docker", append(prefix, args...)...).CombinedOutput()
			require.NoError(t, err, "docker %v: %s", args, b)
			return strings.TrimSpace(string(b))
		}
	}
	t.Fatal("cannot locate fixture's Docker daemon")
	return nil
}

func TestStorageUpgrade(t *testing.T) {
	storageTransition(t, oldApp, harness.PinnedImages[harness.App])
}

func TestStorageDowngrade(t *testing.T) {
	storageTransition(t, harness.PinnedImages[harness.App], oldApp)
}

func storageTransition(t *testing.T, from, to string) {
	old := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Images: map[harness.Component]string{harness.App: from}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := old.Clients(harness.App)
	instance := "upgrade/nested-instance"
	payloads := [][]byte{nil, []byte("small blob\x00with binary bytes"), bytes.Repeat([]byte("large compressed payload\n"), 100000)}
	digests := make([]*repb.Digest, len(payloads))
	for i, b := range payloads {
		var err error
		digests[i], err = c.Write(ctx, instance, b, i == 2)
		require.NoError(t, err)
	}
	action := harness.Digest([]byte("old action cache key"))
	expected := &repb.ActionResult{ExitCode: 0, OutputFiles: []*repb.OutputFile{{Path: "binary.out", Digest: digests[1], IsExecutable: true}}, StdoutDigest: digests[2], StderrRaw: []byte("old stderr\n")}
	_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: action, ActionResult: expected})
	require.NoError(t, err)
	// The server may enrich metadata on UpdateActionResult; compare its
	// pre-upgrade readback, while checking the client-supplied output fields.
	stored, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
	require.NoError(t, err)
	require.Len(t, stored.OutputFiles, 1)
	require.True(t, proto.Equal(expected.OutputFiles[0], stored.OutputFiles[0]))
	require.Equal(t, expected.StderrRaw, stored.StderrRaw)
	expected = stored
	invocationID := uuid.NewString()
	publishInvocation(t, ctx, c, invocationID)
	before := readInvocation(t, old.Web, old.GroupID, invocationID)
	docker := fixtureDocker(t, old)
	volume := docker("inspect", "--format", "{{range .Mounts}}{{if eq .Destination \"/data\"}}{{.Name}}{{end}}{{end}}", old.Endpoint(harness.App).Container)
	require.NotEmpty(t, volume)
	docker("stop", "--time", "30", old.Endpoint(harness.App).Container)
	// Leave the old container owned by its fixture so cleanup preserves its logs.
	// Both fixtures use the same daemon: direct its automatic selection via the
	// standard Docker environment for the duration of the second Start.
	// If private, recover the host from inspect-compatible sockets.
	sockets, _ := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	for _, socket := range sockets {
		if b, err := exec.CommandContext(ctx, "docker", "--host", "unix://"+socket, "inspect", old.Endpoint(harness.App).Container).CombinedOutput(); err == nil && len(b) > 0 {
			t.Setenv("DOCKER_HOST", "unix://"+socket)
			break
		}
	}
	upgraded := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Images: map[harness.Component]string{harness.App: to}, DockerArgs: map[harness.Component][]string{harness.App: {"--mount", "type=volume,source=" + volume + ",target=/data"}}})
	require.Equal(t, old.GroupID, upgraded.GroupID)
	// Reuse the pre-upgrade key to test database/API-key continuity too.
	nc := harness.Dial(t, upgraded.Endpoint(harness.App).GRPC, old.APIKey)
	for i, d := range digests {
		got, err := nc.Read(ctx, instance, d)
		require.NoError(t, err)
		require.True(t, bytes.Equal(payloads[i], got), "blob %d changed", i)
	}
	got, err := nc.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
	require.NoError(t, err)
	require.True(t, proto.Equal(expected, got), "action result changed: %v", got)
	after := readInvocation(t, upgraded.Web, old.GroupID, invocationID)
	require.Equal(t, before, after, "persisted invocation changed across upgrade")
	fresh, err := nc.Upload(ctx, instance, []byte("post-upgrade write"))
	require.NoError(t, err)
	b, err := nc.Read(ctx, instance, fresh)
	require.NoError(t, err)
	require.Equal(t, "post-upgrade write", string(b))
}

// Encode only the stable Bazel BEP fields needed here, from the released
// proto/build_event_stream.proto. No generated BuildBuddy protos are required.
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func number(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func publishInvocation(t *testing.T, ctx context.Context, c *harness.Clients, id string) {
	t.Helper()
	started := field(1, []byte(id))
	started = append(started, number(2, uint64(time.Now().UnixMilli()))...)
	started = append(started, field(3, []byte("8.0.0"))...)
	started = append(started, field(4, []byte("--color=no"))...)
	started = append(started, field(5, []byte("build"))...)
	first := append(field(1, field(3, nil)), field(2, field(9, nil))...)
	first = append(first, field(5, started)...)
	// Supply an actual finish time, as a real Bazel producer does.
	finishPayload := append(field(3, append(field(1, []byte("SUCCESS")), number(2, 0)...)), number(2, uint64(time.Now().UnixMilli()))...)
	finished := append(field(1, field(9, nil)), field(14, finishPayload)...)
	finished = append(finished, number(20, 1)...)
	s, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	streamID := &bespb.StreamId{BuildId: id, InvocationId: id, Component: bespb.StreamId_TOOL}
	for i, event := range [][]byte{first, finished} {
		require.NoError(t, s.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: streamID, SequenceNumber: int64(i + 1), Event: &bespb.BuildEvent{EventTime: timestamppb.Now(), Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: event}}}}}))
	}
	require.NoError(t, s.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: streamID, SequenceNumber: 3, Event: &bespb.BuildEvent{EventTime: timestamppb.Now(), Event: &bespb.BuildEvent_ComponentStreamFinished{ComponentStreamFinished: &bespb.BuildEvent_BuildComponentStreamFinished{Type: bespb.BuildEvent_BuildComponentStreamFinished_FINISHED}}}}}))
	// The released server acknowledges only after EOF and finalization.
	require.NoError(t, s.CloseSend())
	for sequence := int64(1); sequence <= 3; sequence++ {
		ack, err := s.Recv()
		require.NoError(t, err)
		require.Equal(t, sequence, ack.SequenceNumber)
	}
	_, err = s.Recv()
	require.Equal(t, io.EOF, err)
}
func readInvocation(t *testing.T, w *harness.WebSession, group, id string) map[string]any {
	t.Helper()
	var result map[string]any
	require.Eventually(t, func() bool {
		var err error
		result, err = w.RPC("GetInvocation", map[string]any{"requestContext": w.RequestContext(group), "lookup": map[string]any{"invocationId": id}})
		if err != nil {
			return false
		}
		invs, ok := result["invocation"].([]any)
		if !ok || len(invs) != 1 {
			return false
		}
		inv := invs[0].(map[string]any)
		return inv["invocationStatus"] == float64(1) || inv["invocationStatus"] == "COMPLETE_INVOCATION_STATUS"
	}, 30*time.Second, 250*time.Millisecond, "invocation not complete: %v", result)
	inv := result["invocation"].([]any)[0].(map[string]any)
	require.Equal(t, id, inv["invocationId"])
	require.Equal(t, true, inv["success"])
	require.Equal(t, "SUCCESS", inv["bazelExitCode"])
	require.Equal(t, "build", inv["command"])
	require.Len(t, inv["event"], 2)
	// Finalization asynchronously updates the DB timestamp after COMPLETE is
	// visible. Scorecard presence is response enrichment, not persisted BEP.
	delete(inv, "updatedAtUsec")
	if score, ok := inv["scoreCard"].(map[string]any); ok && len(score) == 0 {
		delete(inv, "scoreCard")
	}
	// Compare all remaining metadata and the entire stored event payloads.
	return result
}

func TestAppExecutorVersionSkew(t *testing.T) {
	for _, tc := range []struct{ name, app, executor string }{
		{"new-app-old-executor", harness.PinnedImages[harness.App], oldExecutor},
		{"old-app-new-executor", oldApp, harness.PinnedImages[harness.Executor]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Images: map[harness.Component]string{harness.App: tc.app, harness.Executor: tc.executor}})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			c := f.Clients(harness.App)
			instance := "skew"
			input, err := c.Upload(ctx, instance, []byte("version skew input\n"))
			require.NoError(t, err)
			root, err := c.UploadProto(ctx, instance, &repb.Directory{Files: []*repb.FileNode{{Name: "input", Digest: input}}})
			require.NoError(t, err)
			command, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", "cat input > output; printf 'skew stdout\\n'; printf 'skew stderr\\n' >&2"}, OutputPaths: []string{"output"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
			require.NoError(t, err)
			action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: command, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second)})
			require.NoError(t, err)
			for i := 0; i < 2; i++ {
				s, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: i == 0})
				require.NoError(t, err)
				var response *repb.ExecuteResponse
				for {
					op, err := s.Recv()
					require.NoError(t, err)
					if !op.Done {
						continue
					}
					require.Nil(t, op.GetError())
					response = &repb.ExecuteResponse{}
					require.NoError(t, op.GetResponse().UnmarshalTo(response))
					break
				}
				require.Zero(t, response.GetStatus().GetCode(), "%v", response.GetStatus())
				require.Zero(t, response.GetResult().GetExitCode())
				require.Equal(t, i == 1, response.CachedResult)
				require.Len(t, response.Result.OutputFiles, 1)
				b, err := c.Read(ctx, instance, response.Result.OutputFiles[0].Digest)
				require.NoError(t, err)
				require.Equal(t, "version skew input\n", string(b))
				for _, out := range []struct {
					raw      []byte
					digest   *repb.Digest
					expected string
				}{{response.Result.StdoutRaw, response.Result.StdoutDigest, "skew stdout\n"}, {response.Result.StderrRaw, response.Result.StderrDigest, "skew stderr\n"}} {
					b := out.raw
					if out.digest != nil {
						b, err = c.Read(ctx, instance, out.digest)
						require.NoError(t, err)
					}
					require.Equal(t, out.expected, string(b))
				}
			}
		})
	}
}
