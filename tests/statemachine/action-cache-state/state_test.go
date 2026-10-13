package actioncachestate_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Metadata messages use the released proto/cache.proto and resource.proto wire
// schema. A per-call codec avoids importing BuildBuddy source or generated code.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func bytesField(b []byte, n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, n, protowire.BytesType), v)
}
func intField(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}
func metadataRequest(instance string, d *repb.Digest, kind uint64) []byte {
	db, _ := proto.Marshal(d)
	rn := bytesField(nil, 1, db)
	rn = bytesField(rn, 2, []byte(instance))
	rn = intField(rn, 4, kind)
	rn = intField(rn, 5, 1)
	return bytesField(nil, 2, rn)
}
func metadataSize(t *testing.T, ctx context.Context, c *harness.Clients, method, instance string, d *repb.Digest, kind uint64) int64 {
	t.Helper()
	return metadataValue(t, ctx, c, method, instance, d, kind, 5)
}
func metadataValue(t *testing.T, ctx context.Context, c *harness.Clients, method, instance string, d *repb.Digest, kind uint64, field protowire.Number) int64 {
	t.Helper()
	req := metadataRequest(instance, d, kind)
	var res []byte
	require.NoError(t, c.Conn.Invoke(ctx, method, &req, &res, grpc.ForceCodec(wireCodec{})))
	var size int64
	for len(res) > 0 {
		n, typ, k := protowire.ConsumeTag(res)
		require.Positive(t, k)
		res = res[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(res)
			require.Positive(t, k)
			if n == field {
				size = int64(v)
			}
			res = res[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, res)
			require.Positive(t, k)
			res = res[k:]
		}
	}
	return size
}

func TestActionResultStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy},
		Config: map[harness.Component]harness.Config{harness.App: {"cache": harness.Config{"pebble": harness.Config{"min_bytes_auto_zstd_compression": int64(1 << 20)}}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	app := f.Clients(harness.App)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			c := f.Clients(component)
			instance := "state/" + string(component)
			input := []byte("valid action input\n")
			id, err := c.Upload(ctx, instance, input)
			require.NoError(t, err)
			root, err := c.UploadProto(ctx, instance, &repb.Directory{Files: []*repb.FileNode{{Name: "input.txt", Digest: id}}})
			require.NoError(t, err)
			cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", "cat input.txt > result.txt"}, OutputPaths: []string{"result.txt"}})
			require.NoError(t, err)
			action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root})
			require.NoError(t, err)
			_, err = c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
			require.Equal(t, codes.NotFound, status.Code(err))
			type blobState struct {
				d *repb.Digest
				b []byte
			}
			history := []blobState{{id, input}}
			for _, d := range []*repb.Digest{root, cmd, action} {
				b, err := c.Read(ctx, instance, d)
				require.NoError(t, err)
				require.True(t, proto.Equal(d, harness.Digest(b)))
				history = append(history, blobState{d, b})
			}
			// The reference model contains the accepted revision and its immutable blobs.
			for _, revision := range []int{1, 1, 2, 2, 3} {
				payload := []byte(fmt.Sprintf("revision %d output with different length %s\n", revision, strings.Repeat("x", revision*7)))
				stdout := []byte(fmt.Sprintf("stdout %d\n", revision))
				stderr := []byte(fmt.Sprintf("stderr %d\n", revision))
				od, err := c.Write(ctx, instance, payload, false)
				require.NoError(t, err)
				sd, err := c.Upload(ctx, instance, stdout)
				require.NoError(t, err)
				ed, err := c.Upload(ctx, instance, stderr)
				require.NoError(t, err)
				model := &repb.ActionResult{ExitCode: int32(revision - 1), OutputFiles: []*repb.OutputFile{{Path: "result.txt", Digest: od, IsExecutable: revision == 3}}, StdoutDigest: sd, StderrDigest: ed}
				if revision == 3 {
					model.StdoutRaw = stdout
					model.StdoutDigest = nil
					model.StderrRaw = stderr
					model.StderrDigest = nil
				}
				history = append(history, blobState{od, payload}, blobState{sd, stdout}, blobState{ed, stderr})
				accepted, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: action, ActionResult: model})
				require.NoError(t, err)
				require.Equal(t, model.ExitCode, accepted.ExitCode)
				require.Len(t, accepted.OutputFiles, 1)
				require.True(t, proto.Equal(model.OutputFiles[0], accepted.OutputFiles[0]))
				for projection := 0; projection < 8; projection++ {
					inline := projection&1 != 0
					req := &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action, InlineStdout: projection&2 != 0, InlineStderr: projection&4 != 0}
					if inline {
						req.InlineOutputFiles = []string{"result.txt"}
					}
					got, err := c.AC.GetActionResult(ctx, req)
					require.NoError(t, err)
					require.Equal(t, model.ExitCode, got.ExitCode)
					require.Len(t, got.OutputFiles, 1)
					require.Equal(t, model.OutputFiles[0].Path, got.OutputFiles[0].Path)
					require.True(t, proto.Equal(od, got.OutputFiles[0].Digest))
					require.Equal(t, model.OutputFiles[0].IsExecutable, got.OutputFiles[0].IsExecutable)
					if inline {
						require.Equal(t, payload, got.OutputFiles[0].Contents)
					} else {
						require.Empty(t, got.OutputFiles[0].Contents)
					}
					checkStream(t, ctx, c, instance, stdout, got.StdoutRaw, got.StdoutDigest)
					checkStream(t, ctx, c, instance, stderr, got.StderrRaw, got.StderrDigest)
				}
				// CAS objects are immutable: old revisions remain readable after replacement.
				for _, blob := range history {
					b, err := c.Read(ctx, instance, blob.d)
					require.NoError(t, err)
					require.Equal(t, blob.b, b)
					batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{blob.d}, DigestFunction: repb.DigestFunction_SHA256})
					require.NoError(t, err)
					require.Len(t, batch.Responses, 1)
					require.Zero(t, batch.Responses[0].Status.Code)
					require.Equal(t, blob.b, batch.Responses[0].Data)
					missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{blob.d}})
					require.NoError(t, err)
					require.Empty(t, missing.MissingBlobDigests)
					require.Equal(t, blob.d.SizeBytes, metadataSize(t, ctx, c, "/cache.service.Cache/GetMetadata", instance, blob.d, 2))
					require.Equal(t, blob.d.SizeBytes, metadataSize(t, ctx, app, "/cache.service.Cache/GetMetadata", instance, blob.d, 2))
					require.Equal(t, blob.d.SizeBytes, metadataSize(t, ctx, app, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", instance, blob.d, 2))
					ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": blob.d.Hash, "sizeBytes": strconv.FormatInt(blob.d.SizeBytes, 10)}, "instanceName": instance, "cacheType": 2, "digestFunction": 1}})
					require.NoError(t, err)
					require.Equal(t, strconv.FormatInt(blob.d.SizeBytes, 10), fmt.Sprint(ui["digestSizeBytes"]))
				}
				// DigestSizeBytes names the Action; StoredSizeBytes measures the result.
				// These small results are below the configured compression threshold.
				wantSize := int64(proto.Size(accepted))
				require.Equal(t, action.SizeBytes, metadataSize(t, ctx, app, "/cache.service.Cache/GetMetadata", instance, action, 1))
				require.Equal(t, wantSize, metadataValue(t, ctx, app, "/cache.service.Cache/GetMetadata", instance, action, 1, 2))
				require.Equal(t, action.SizeBytes, metadataSize(t, ctx, app, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", instance, action, 1))
				require.Equal(t, wantSize, metadataValue(t, ctx, app, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", instance, action, 1, 2))
				ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": action.Hash, "sizeBytes": strconv.FormatInt(action.SizeBytes, 10)}, "instanceName": instance, "cacheType": 1, "digestFunction": 1}})
				require.NoError(t, err)
				require.Equal(t, strconv.FormatInt(action.SizeBytes, 10), fmt.Sprint(ui["digestSizeBytes"]))
				require.Equal(t, strconv.FormatInt(wantSize, 10), fmt.Sprint(ui["storedSizeBytes"]))
				output := filepath.Join(t.TempDir(), "download")
				cli := exec.CommandContext(ctx, "bb", "download", harness.ResourceName(instance, od, false), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+output)
				cli.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
				log, err := cli.CombinedOutput()
				require.NoError(t, err, "bb download: %s", log)
				b, err := os.ReadFile(output)
				require.NoError(t, err)
				require.Equal(t, payload, b)
			}
		})
	}
}

func checkStream(t *testing.T, ctx context.Context, c *harness.Clients, instance string, want, raw []byte, d *repb.Digest) {
	t.Helper()
	if len(raw) > 0 {
		require.Equal(t, want, raw)
		return
	}
	require.NotNil(t, d)
	b, err := c.Read(ctx, instance, d)
	require.NoError(t, err)
	require.Equal(t, want, b)
}
