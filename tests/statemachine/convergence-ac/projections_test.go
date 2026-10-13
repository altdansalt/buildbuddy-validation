package convergence_test

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTypedCLIProjection(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	command, err := uploadCheckedProto(t, ctx, f, clients, &repb.Command{Arguments: []string{"true"}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "MODEL", Value: "typed-json"}}})
	require.NoError(t, err)
	root, err := uploadCheckedProto(t, ctx, f, clients, &repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "target"}}})
	require.NoError(t, err)
	for _, item := range []struct {
		name string
		msg  proto.Message
	}{
		{"Command", &repb.Command{Arguments: []string{"true"}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "MODEL", Value: "typed-json"}}}},
		{"Directory", &repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "target"}}}},
		{"Action", &repb.Action{CommandDigest: command, InputRootDigest: root}},
	} {
		b, d, err := harness.MarshalDigest(item.msg)
		require.NoError(t, err)
		_, err = clients[1].Write(ctx, "", b, false)
		require.NoError(t, err)
		checkBlobSurfaces(t, ctx, f, clients, d, b)
		for _, side := range []harness.Component{harness.App, harness.CacheProxy} {
			cmd := exec.CommandContext(ctx, "bb", "download", "--target=grpc://"+f.Endpoint(side).GRPC, "--api_key="+f.APIKey, "--type="+item.name, d.Hash+"/"+strconv.FormatInt(d.SizeBytes, 10))
			cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
			out, err := cmd.Output()
			require.NoError(t, err)
			got := item.msg.ProtoReflect().Type().New().Interface()
			require.NoError(t, protojson.Unmarshal(out, got))
			require.True(t, proto.Equal(item.msg, got), "%s typed CLI JSON", item.name)
		}
	}
}

func TestExecutionMetadataProjection(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	command, err := uploadCheckedProto(t, ctx, f, clients, &repb.Command{Arguments: []string{"true"}})
	require.NoError(t, err)
	root, err := uploadCheckedProto(t, ctx, f, clients, &repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "target"}}})
	require.NoError(t, err)
	action, err := uploadCheckedProto(t, ctx, f, clients, &repb.Action{CommandDigest: command, InputRootDigest: root})
	require.NoError(t, err)
	b := []byte("metadata output")
	d, err := clients[1].Upload(ctx, "", b)
	require.NoError(t, err)
	checkBlobSurfaces(t, ctx, f, clients, d, b)
	for step := 0; step < 2; step++ {
		ts := func(n int64) *timestamppb.Timestamp {
			return timestamppb.New(time.Unix(1700000000+int64(step)*10+n, 123000000))
		}
		aux, err := anypb.New(&emptypb.Empty{})
		require.NoError(t, err)
		m := model{digests: []*repb.Digest{d}, blobs: map[string][]byte{d.Hash: b}, result: &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "result.txt", Digest: d}}, ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: "caller-controlled", QueuedTimestamp: ts(0), WorkerStartTimestamp: ts(1), InputFetchStartTimestamp: ts(2), InputFetchCompletedTimestamp: ts(3), ExecutionStartTimestamp: ts(4), ExecutionCompletedTimestamp: ts(5), OutputUploadStartTimestamp: ts(6), OutputUploadCompletedTimestamp: ts(7), WorkerCompletedTimestamp: ts(8), AuxiliaryMetadata: []*anypb.Any{aux}}}}
		result, err := clients[step].AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: action, ActionResult: proto.Clone(m.result).(*repb.ActionResult), DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		require.True(t, proto.Equal(m.result, result))
		started := time.Now()
		require.Eventually(t, func() bool { return checkReads(ctx, clients, action, m) == nil && checkHTTP(ctx, f, action, m) == nil }, 15*time.Second, 100*time.Millisecond)
		t.Logf("full execution metadata step %d converged in %s", step, time.Since(started))
		checkACMetadataPair(t, ctx, f, clients[0], action)
		for stable := 0; stable < 3; stable++ {
			time.Sleep(500 * time.Millisecond)
			require.NoError(t, checkReads(ctx, clients, action, m))
			require.NoError(t, checkHTTP(ctx, f, action, m))
			checkACMetadataPair(t, ctx, f, clients[0], action)
		}
	}
}
