package digestexecution_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
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
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

var seed = flag.Int64("digest.seed", 314159, "Replay seed for digest execution properties")
var iterations = flag.Int("digest.iterations", 6, "Cases per digest function (1..100)")
var functions = []repb.DigestFunction_Value{repb.DigestFunction_SHA256, repb.DigestFunction_SHA1, repb.DigestFunction_SHA384, repb.DigestFunction_SHA512, repb.DigestFunction_BLAKE3}

const instance = "digest-function-execution"

func digest(b []byte, df repb.DigestFunction_Value) *repb.Digest {
	var sum []byte
	switch df {
	case repb.DigestFunction_SHA1:
		h := sha1.Sum(b)
		sum = h[:]
	case repb.DigestFunction_SHA256:
		h := sha256.Sum256(b)
		sum = h[:]
	case repb.DigestFunction_SHA384:
		h := sha512.Sum384(b)
		sum = h[:]
	case repb.DigestFunction_SHA512:
		h := sha512.Sum512(b)
		sum = h[:]
	case repb.DigestFunction_BLAKE3:
		sum = blake3(b)
	default:
		panic(df)
	}
	return &repb.Digest{Hash: hex.EncodeToString(sum), SizeBytes: int64(len(b))}
}
func upload(t *testing.T, ctx context.Context, c *harness.Clients, df repb.DigestFunction_Value, b []byte) *repb.Digest {
	t.Helper()
	d := digest(b, df)
	r, e := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: df, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: b}}})
	require.NoError(t, e)
	require.Len(t, r.Responses, 1)
	require.Zero(t, r.Responses[0].GetStatus().GetCode(), "upload %s: %v", df, r)
	return d
}
func uploadProto(t *testing.T, ctx context.Context, c *harness.Clients, df repb.DigestFunction_Value, m proto.Message) *repb.Digest {
	t.Helper()
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	require.NoError(t, e)
	return upload(t, ctx, c, df, b)
}
func read(t *testing.T, ctx context.Context, c *harness.Clients, df repb.DigestFunction_Value, d *repb.Digest) []byte {
	t.Helper()
	r, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, DigestFunction: df, Digests: []*repb.Digest{d}})
	require.NoError(t, e)
	require.Len(t, r.Responses, 1)
	require.Zero(t, r.Responses[0].GetStatus().GetCode(), "read %s: %v", df, r)
	b := append([]byte{}, r.Responses[0].Data...)
	require.True(t, proto.Equal(d, digest(b, df)), "digest %v does not hash its bytes with %s", d, df)
	segment := ""
	if df == repb.DigestFunction_BLAKE3 {
		segment = "blake3/"
	}
	stream, e := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: fmt.Sprintf("%s/blobs/%s%s/%d", instance, segment, d.Hash, d.SizeBytes)})
	require.NoError(t, e)
	var wire []byte
	for {
		part, e := stream.Recv()
		if e == io.EOF {
			break
		}
		require.NoError(t, e)
		wire = append(wire, part.Data...)
	}
	require.True(t, bytes.Equal(b, wire), "ByteStream and BatchRead disagree for %s", df)
	return b
}
func execute(ctx context.Context, c *harness.Clients, df repb.DigestFunction_Value, d *repb.Digest, skip bool) (*repb.ExecuteResponse, error) {
	s, e := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, DigestFunction: df, ActionDigest: d, SkipCacheLookup: skip})
	if e != nil {
		return nil, e
	}
	for {
		op, e := s.Recv()
		if e == io.EOF {
			return nil, fmt.Errorf("execution stream ended without completion")
		}
		if e != nil {
			return nil, e
		}
		if !op.Done {
			continue
		}
		if op.GetError() != nil {
			return nil, status.ErrorProto(op.GetError())
		}
		r := new(repb.ExecuteResponse)
		if e := op.GetResponse().UnmarshalTo(r); e != nil {
			return nil, e
		}
		return r, nil
	}
}
func action(t *testing.T, ctx context.Context, proxy *harness.Clients, df repb.DigestFunction_Value, payload []byte, salt int64) *repb.Digest {
	input := upload(t, ctx, proxy, df, payload)
	root := uploadProto(t, ctx, proxy, df, &repb.Directory{Files: []*repb.FileNode{{Name: "in", Digest: input}}})
	cmd := uploadProto(t, ctx, proxy, df, &repb.Command{Arguments: []string{"/bin/sh", "-c", "mkdir -p tree/sub; cat in > out; cat in > tree/sub/copy; printf 'stdout'; printf 'stderr' >&2"}, OutputPaths: []string{"out", "tree"}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "CASE_SALT", Value: fmt.Sprint(salt)}}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "Arch", Value: "amd64"}, {Name: "OSFamily", Value: "linux"}, {Name: "workload-isolation-type", Value: "none"}}}})
	return uploadProto(t, ctx, proxy, df, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(20 * time.Second)})
}
func checkResult(t *testing.T, ctx context.Context, proxy *harness.Clients, df repb.DigestFunction_Value, r *repb.ActionResult, payload []byte) {
	t.Helper()
	require.NotNil(t, r)
	require.Zero(t, r.ExitCode)
	require.Len(t, r.OutputFiles, 1)
	require.Equal(t, "out", r.OutputFiles[0].Path)
	require.Equal(t, payload, read(t, ctx, proxy, df, r.OutputFiles[0].Digest))
	require.True(t, proto.Equal(digest(payload, df), r.OutputFiles[0].Digest))
	require.Len(t, r.OutputDirectories, 1)
	require.Equal(t, "tree", r.OutputDirectories[0].Path)
	tree := new(repb.Tree)
	require.NoError(t, proto.Unmarshal(read(t, ctx, proxy, df, r.OutputDirectories[0].TreeDigest), tree))
	require.Len(t, tree.Root.Directories, 1)
	require.Equal(t, "sub", tree.Root.Directories[0].Name)
	require.Len(t, tree.Children, 1)
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(tree.Children[0])
	require.NoError(t, e)
	require.True(t, proto.Equal(digest(b, df), tree.Root.Directories[0].Digest), "Tree DirectoryNode digest uses wrong function")
	require.Len(t, tree.Children[0].Files, 1)
	require.Equal(t, "copy", tree.Children[0].Files[0].Name)
	require.Equal(t, payload, read(t, ctx, proxy, df, tree.Children[0].Files[0].Digest))
	if d := r.OutputDirectories[0].RootDirectoryDigest; d != nil {
		b, e := proto.MarshalOptions{Deterministic: true}.Marshal(tree.Root)
		require.NoError(t, e)
		require.True(t, proto.Equal(d, digest(b, df)))
	}
	stdout, stderr := r.StdoutRaw, r.StderrRaw
	if r.StdoutDigest != nil {
		stdout = read(t, ctx, proxy, df, r.StdoutDigest)
	}
	if r.StderrDigest != nil {
		stderr = read(t, ctx, proxy, df, r.StderrDigest)
	}
	require.Equal(t, []byte("stdout"), stdout)
	require.Equal(t, []byte("stderr"), stderr)
}

// Register before Start, so this runs after the harness preserves logs.
func start(t *testing.T) *harness.Fixture {
	var f *harness.Fixture
	t.Cleanup(func() {
		if f == nil {
			return
		}
		for _, component := range []harness.Component{harness.App, harness.Executor, harness.CacheProxy} {
			path := filepath.Join(f.OutputDir, f.Endpoint(component).Container+".logs.txt")
			b, e := os.ReadFile(path)
			require.NoError(t, e)
			require.False(t, strings.Contains(string(b), "panic:") || strings.Contains(string(b), "fatal error:"), "seed=%d component=%s panic in preserved log %s", *seed, component, path)
		}
	})
	f = harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor, harness.CacheProxy}})
	return f
}
func TestDigestExecutionProperties(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	require.GreaterOrEqual(t, *iterations, 1)
	require.LessOrEqual(t, *iterations, 100)
	f := start(t)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	caps, e := app.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, e)
	require.True(t, caps.GetExecutionCapabilities().GetExecEnabled())
	pcaps, e := proxy.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, e)
	rng := rand.New(rand.NewSource(*seed))
	sizes := []int{0, 1, 63, 64, 1024, 4097}
	for i := 0; i < *iterations; i++ {
		n := sizes[i%len(sizes)]
		if i >= len(sizes) {
			n = rng.Intn(16385)
		}
		payload := make([]byte, n)
		rng.Read(payload)
		if i%3 == 0 {
			payload = bytes.Repeat([]byte{byte(rng.Intn(256))}, n)
		}
		for _, df := range functions {
			t.Run(fmt.Sprintf("seed=%d/case=%d/size=%d/%s", *seed, i, n, df), func(t *testing.T) {
				require.Contains(t, caps.ExecutionCapabilities.DigestFunctions, df)
				require.Contains(t, pcaps.CacheCapabilities.DigestFunctions, df)
				d := action(t, ctx, proxy, df, payload, *seed+int64(i))
				r, e := execute(ctx, app, df, d, true)
				require.NoError(t, e)
				require.Zero(t, r.GetStatus().GetCode(), "%v", r.GetStatus())
				checkResult(t, ctx, proxy, df, r.Result, payload)
				for _, c := range []*harness.Clients{app, proxy} {
					ac, e := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, DigestFunction: df, ActionDigest: d})
					require.NoError(t, e)
					checkResult(t, ctx, proxy, df, ac, payload)
				}
				cached, e := execute(ctx, app, df, d, false)
				require.NoError(t, e)
				require.Zero(t, cached.GetStatus().GetCode(), "%v", cached.GetStatus())
				require.True(t, cached.CachedResult)
				checkResult(t, ctx, proxy, df, cached.Result, payload)
				for _, other := range functions {
					if other == df {
						continue
					}
					_, e := proxy.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, DigestFunction: other, ActionDigest: d})
					require.Contains(t, []codes.Code{codes.NotFound, codes.InvalidArgument}, status.Code(e), "AC key incorrectly accepted under %s instead of %s", other, df)
				}
			})
		}
	}
}
func TestMixedActionDigestsRejected(t *testing.T) {
	t.Logf("seed=%d (fixed minimal mixed-function case)", *seed)
	f := start(t)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := uploadProto(t, ctx, proxy, repb.DigestFunction_SHA256, &repb.Directory{})
	cmd := uploadProto(t, ctx, proxy, repb.DigestFunction_SHA1, &repb.Command{Arguments: []string{"/bin/true"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "workload-isolation-type", Value: "none"}}}})
	d := uploadProto(t, ctx, proxy, repb.DigestFunction_SHA256, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(10 * time.Second)})
	r, e := execute(ctx, app, repb.DigestFunction_SHA256, d, true)
	code := status.Code(e)
	if e == nil {
		code = codes.Code(r.GetStatus().GetCode())
	}
	require.Contains(t, []codes.Code{codes.InvalidArgument, codes.FailedPrecondition}, code, "mixed SHA1 command in SHA256 action must be rejected cleanly: response=%v err=%v", r, e)
}

func TestMixedInputDigestsRejected(t *testing.T) {
	t.Logf("seed=%d (fixed mixed-function input cases)", *seed)
	f := start(t)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, other := range functions {
		if other == repb.DigestFunction_SHA256 {
			continue
		}
		t.Run(other.String(), func(t *testing.T) {
			wrong := upload(t, ctx, proxy, other, []byte("x"))
			root := uploadProto(t, ctx, proxy, repb.DigestFunction_SHA256, &repb.Directory{Files: []*repb.FileNode{{Name: "in", Digest: wrong}}})
			cmd := uploadProto(t, ctx, proxy, repb.DigestFunction_SHA256, &repb.Command{Arguments: []string{"/bin/sh", "-c", "cat in"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "Arch", Value: "amd64"}, {Name: "OSFamily", Value: "linux"}, {Name: "workload-isolation-type", Value: "none"}}}})
			d := uploadProto(t, ctx, proxy, repb.DigestFunction_SHA256, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(10 * time.Second)})
			r, e := execute(ctx, app, repb.DigestFunction_SHA256, d, true)
			code := status.Code(e)
			if e == nil {
				code = codes.Code(r.GetStatus().GetCode())
			}
			require.Contains(t, []codes.Code{codes.InvalidArgument, codes.FailedPrecondition}, code, "mixed %s file in SHA256 input must be rejected cleanly: response=%v err=%v", other, r, e)
		})
	}
}
