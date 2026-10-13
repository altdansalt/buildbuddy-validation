package actioncachestate_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	"google.golang.org/protobuf/encoding/protojson"
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
		Config: map[harness.Component]harness.Config{harness.App: {"cache": harness.Config{"pebble": harness.Config{"min_bytes_auto_zstd_compression": int64(1 << 20)}}}, harness.CacheProxy: {"cache": harness.Config{"pebble": harness.Config{"min_bytes_auto_zstd_compression": int64(1 << 20)}}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	app := f.Clients(harness.App)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			c := f.Clients(component)
			instance := "state/shared/" + string(component)
			var currentAction *repb.Digest
			var currentResult *repb.ActionResult
			directories := map[string]*repb.Directory{}
			directoryDigests := map[string]*repb.Digest{}
			var blobs []struct {
				d *repb.Digest
				b []byte
			}
			put := func(data []byte, batch bool) (*repb.Digest, error) {
				var d *repb.Digest
				var err error
				if batch {
					d, err = c.Upload(ctx, instance, data)
				} else {
					d, err = c.Write(ctx, instance, data, false)
				}
				if err != nil {
					return nil, err
				}
				seen := false
				for _, blob := range blobs {
					if proto.Equal(blob.d, d) {
						seen = true
						require.Equal(t, blob.b, data)
					}
				}
				if !seen {
					blobs = append(blobs, struct {
						d *repb.Digest
						b []byte
					}{d, append([]byte(nil), data...)})
				}
				for _, blob := range blobs {
					checkBlobSurfaces(t, ctx, f, instance, blob.d, blob.b)
				}
				for _, hash := range orderedKeys(directories) {
					checkSingleDirectory(t, ctx, f, instance, directoryDigests[hash], directories[hash])
				}
				if currentAction != nil {
					checkActionSurfaces(t, ctx, f, instance, currentAction, currentResult)
				}
				return d, nil
			}
			putProto := func(m proto.Message) (*repb.Digest, error) {
				b, err := proto.Marshal(m)
				if err != nil {
					return nil, err
				}
				d, err := put(b, true)
				if err == nil {
					kind := ""
					switch m.(type) {
					case *repb.Action:
						kind = "Action"
					case *repb.Command:
						kind = "Command"
					case *repb.Directory:
						kind = "Directory"
					}
					for _, endpoint := range []harness.Component{harness.App, harness.CacheProxy} {
						out := filepath.Join(t.TempDir(), "typed")
						cli := exec.CommandContext(ctx, "bb", "download", harness.ResourceName(instance, d, false), "--type="+kind, "--target=grpc://"+f.Endpoint(endpoint).GRPC, "--api_key="+f.APIKey, "--output_file="+out)
						cli.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
						log, err := cli.CombinedOutput()
						require.NoError(t, err, "%s", log)
						data, err := os.ReadFile(out)
						require.NoError(t, err)
						actual := m.ProtoReflect().Type().New().Interface()
						require.NoError(t, protojson.Unmarshal(data, actual))
						require.True(t, proto.Equal(m, actual))
					}
					if dir, ok := m.(*repb.Directory); ok {
						directories[d.Hash] = proto.Clone(dir).(*repb.Directory)
						directoryDigests[d.Hash] = d
						checkSingleDirectory(t, ctx, f, instance, d, dir)
					}
				}
				return d, err
			}

			input := []byte("valid action input\n")
			id, err := put(input, true)
			require.NoError(t, err)
			root, err := putProto(&repb.Directory{Files: []*repb.FileNode{{Name: "input.txt", Digest: id}}})
			require.NoError(t, err)
			cmd, err := putProto(&repb.Command{Arguments: []string{"/bin/sh", "-c", "cat input.txt > result.txt"}, OutputPaths: []string{"result.txt"}})
			require.NoError(t, err)
			action, err := putProto(&repb.Action{CommandDigest: cmd, InputRootDigest: root})
			require.NoError(t, err)
			currentAction = action
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
				od, err := put(payload, false)
				require.NoError(t, err)
				sd, err := put(stdout, true)
				require.NoError(t, err)
				ed, err := put(stderr, true)
				require.NoError(t, err)
				model := &repb.ActionResult{ExitCode: int32(revision - 1), OutputFiles: []*repb.OutputFile{{Path: "result.txt", Digest: od, IsExecutable: revision == 3}}, StdoutDigest: sd, StderrDigest: ed, ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: "reference-worker"}, OutputFileSymlinks: []*repb.OutputSymlink{{Path: "file-link", Target: "result.txt"}}, OutputDirectorySymlinks: []*repb.OutputSymlink{{Path: "dir-link", Target: "outputs"}}}
				if revision == 3 {
					model.StdoutRaw = stdout
					model.StdoutDigest = nil
					model.StderrRaw = stderr
					model.StderrDigest = nil
				}
				history = append(history, blobState{od, payload}, blobState{sd, stdout}, blobState{ed, stderr})
				accepted, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: action, ActionResult: model})
				require.NoError(t, err)
				require.True(t, proto.Equal(model, accepted), "UpdateActionResult response")
				currentResult = proto.Clone(model).(*repb.ActionResult)
				checkActionSurfaces(t, ctx, f, instance, action, currentResult)
				for _, hash := range orderedKeys(directories) {
					checkSingleDirectory(t, ctx, f, instance, directoryDigests[hash], directories[hash])
				}
				for _, blob := range blobs {
					checkBlobSurfaces(t, ctx, f, instance, blob.d, blob.b)
				}
				rawResult := download(t, ctx, f, fmt.Sprintf("actioncache://app:1985/%s/blobs/ac/%s/%d", instance, action.Hash, action.SizeBytes))
				httpResult := &repb.ActionResult{}
				require.NoError(t, proto.Unmarshal(rawResult, httpResult))
				require.True(t, proto.Equal(model, httpResult))
				require.Equal(t, model.ExitCode, accepted.ExitCode)
				require.Len(t, accepted.OutputFiles, 1)
				require.True(t, proto.Equal(model.OutputFiles[0], accepted.OutputFiles[0]))
				for _, reader := range []*harness.Clients{app, f.Clients(harness.CacheProxy)} {
					full, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
					require.NoError(t, err)
					require.True(t, proto.Equal(model, full), "complete action result: want %s got %s", model, full)
				}
				for _, projectionReader := range []*harness.Clients{app, f.Clients(harness.CacheProxy)} {
					for projection := 0; projection < 8; projection++ {
						inline := projection&1 != 0
						req := &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action, InlineStdout: projection&2 != 0, InlineStderr: projection&4 != 0}
						if inline {
							req.InlineOutputFiles = []string{"result.txt"}
						}
						got, err := projectionReader.AC.GetActionResult(ctx, req)
						require.NoError(t, err)
						expected := proto.Clone(model).(*repb.ActionResult)
						if inline {
							expected.OutputFiles[0].Contents = payload
						}
						require.True(t, proto.Equal(expected, got), "complete inline projection: %s", got)
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
						require.Equal(t, model.StdoutRaw, got.StdoutRaw)
						require.True(t, proto.Equal(model.StdoutDigest, got.StdoutDigest))
						checkStream(t, ctx, projectionReader, instance, stdout, got.StdoutRaw, got.StdoutDigest)
						require.Equal(t, model.StderrRaw, got.StderrRaw)
						require.True(t, proto.Equal(model.StderrDigest, got.StderrDigest))
						checkStream(t, ctx, projectionReader, instance, stderr, got.StderrRaw, got.StderrDigest)
					}
				}
				// CAS objects are immutable: old revisions remain readable after replacement.
				for _, blob := range history {
					b, err := c.Read(ctx, instance, blob.d)
					require.NoError(t, err)
					require.Equal(t, blob.b, b)
					for _, reader := range []*harness.Clients{app, f.Clients(harness.CacheProxy)} {
						other, err := reader.Read(ctx, instance, blob.d)
						require.NoError(t, err)
						require.Equal(t, blob.b, other)
					}
					require.Equal(t, blob.b, download(t, ctx, f, "bytestream://app:1985/"+harness.ResourceName(instance, blob.d, false)))
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
					require.Equal(t, blob.d.SizeBytes, metadataValue(t, ctx, app, "/cache.service.Cache/GetMetadata", instance, blob.d, 2, 2))
					require.Positive(t, metadataValue(t, ctx, app, "/cache.service.Cache/GetMetadata", instance, blob.d, 2, 3))
					require.Positive(t, metadataValue(t, ctx, app, "/cache.service.Cache/GetMetadata", instance, blob.d, 2, 4))
					checkPublicFile(t, ctx, f, app, instance, blob.d, blob.b)
					require.Equal(t, blob.d.SizeBytes, metadataSize(t, ctx, app, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", instance, blob.d, 2))
					ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": blob.d.Hash, "sizeBytes": strconv.FormatInt(blob.d.SizeBytes, 10)}, "instanceName": instance, "cacheType": 2, "digestFunction": 1}})
					require.NoError(t, err)
					require.Equal(t, strconv.FormatInt(blob.d.SizeBytes, 10), fmt.Sprint(ui["digestSizeBytes"]))
				}
				// DigestSizeBytes names the Action; StoredSizeBytes measures the result.
				// These small results are below the configured compression threshold.
				wantSize := int64(proto.Size(model))
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

// Audit: share one action across endpoints; warm all proxy projections before overwrite.
func TestCrossSurfaceOverwriteDeleteRecreate(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	app := f.Clients(harness.App)
	key := f.Web.CreateAPIKey(t, f.GroupID, "second-action-client", []string{"CACHE_WRITE"})
	proxy := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key)
	instance := "audit/shared"
	history := map[string][]byte{}
	digests := map[string]*repb.Digest{}
	var current *repb.ActionResult
	cmd, err := app.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"true"}})
	require.NoError(t, err)
	root, err := app.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	action, err := app.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root})
	require.NoError(t, err)
	for revision, writer := range []*harness.Clients{app, app, proxy, app} {
		payload := []byte(fmt.Sprintf("audit-output-%d", revision))
		od, err := writer.Write(ctx, instance, payload, false)
		require.NoError(t, err)
		history[od.Hash] = payload
		digests[od.Hash] = od
		for _, hash := range orderedKeys(history) {
			checkBlobSurfaces(t, ctx, f, instance, digests[hash], history[hash])
		}
		checkActionSurfaces(t, ctx, f, instance, action, current)
		want := &repb.ActionResult{ExitCode: int32(revision), OutputFiles: []*repb.OutputFile{{Path: "out", Digest: od}}, StdoutRaw: payload, StderrRaw: payload, ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: "reference-worker"}}
		_, err = writer.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: action, ActionResult: want})
		require.NoError(t, err)
		current = proto.Clone(want).(*repb.ActionResult)
		checkActionSurfaces(t, ctx, f, instance, action, current)
		for _, reader := range []*harness.Clients{app, proxy} {
			for projection := 0; projection < 8; projection++ {
				req := &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action, InlineStdout: projection&2 != 0, InlineStderr: projection&4 != 0}
				if projection&1 != 0 {
					req.InlineOutputFiles = []string{"out"}
				}
				got, err := reader.AC.GetActionResult(ctx, req)
				require.NoError(t, err)
				expected := proto.Clone(want).(*repb.ActionResult)
				if projection&1 != 0 {
					expected.OutputFiles[0].Contents = payload
				}
				require.True(t, proto.Equal(expected, got), "complete projection: %s", got)
				require.Equal(t, want.ExitCode, got.ExitCode)
				require.Len(t, got.OutputFiles, 1)
				require.True(t, proto.Equal(od, got.OutputFiles[0].Digest))
				require.Equal(t, payload, got.StdoutRaw)
				require.Equal(t, payload, got.StderrRaw)
				if projection&1 != 0 {
					require.Equal(t, payload, got.OutputFiles[0].Contents)
				}
			}
			b, err := reader.Read(ctx, instance, od)
			require.NoError(t, err)
			require.Equal(t, payload, b)
		}
		for _, kind := range []string{"actioncache", "bytestream"} {
			resource := fmt.Sprintf("actioncache://app:1985/%s/blobs/ac/%s/%d", instance, action.Hash, action.SizeBytes)
			if kind == "bytestream" {
				resource = "bytestream://app:1985/" + harness.ResourceName(instance, od, false)
			}
			req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?bytestream_url="+url.QueryEscape(resource), nil)
			require.NoError(t, err)
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			res, err := f.Web.Client.Do(req)
			require.NoError(t, err)
			b, err := io.ReadAll(res.Body)
			res.Body.Close()
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode, "%s", b)
			if kind == "bytestream" {
				require.Equal(t, payload, b)
			} else {
				got := &repb.ActionResult{}
				require.NoError(t, proto.Unmarshal(b, got))
				require.True(t, proto.Equal(want, got), "HTTP complete result: %s", got)
				require.Equal(t, want.ExitCode, got.ExitCode)
				require.Len(t, got.OutputFiles, 1)
				require.True(t, proto.Equal(od, got.OutputFiles[0].Digest))
				require.Equal(t, payload, got.StdoutRaw)
				require.Equal(t, payload, got.StderrRaw)
			}
		}
		if revision == 2 {
			req := bytesField(nil, 1, []byte(fmt.Sprintf("%s/blobs/ac/%s/%d", instance, action.Hash, action.SizeBytes)))
			var res []byte
			require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &req, &res, grpc.ForceCodec(wireCodec{})))
			started := time.Now()
			for _, reader := range []*harness.Clients{app, proxy} {
				require.Eventually(t, func() bool {
					_, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
					return status.Code(err) == codes.NotFound
				}, 10*time.Second, 100*time.Millisecond)
				for stable := 0; stable < 3; stable++ {
					time.Sleep(100 * time.Millisecond)
					_, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
					require.Equal(t, codes.NotFound, status.Code(err))
				}
			}
			current = nil
			checkActionSurfaces(t, ctx, f, instance, action, nil)
			for _, hash := range orderedKeys(history) {
				checkBlobSurfaces(t, ctx, f, instance, digests[hash], history[hash])
			}
			t.Logf("AC delete convergence including three stable reads: %s", time.Since(started))
		}
		if revision != 2 {
			for stable := 0; stable < 3; stable++ {
				time.Sleep(100 * time.Millisecond)
				for _, reader := range []*harness.Clients{app, proxy} {
					got, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
					require.NoError(t, err)
					require.True(t, proto.Equal(want, got))
				}
			}
		}
		t.Logf("revision %d: both endpoints, all inline projections, HTTP AC and CAS agree", revision)
	}
}

func download(t *testing.T, ctx context.Context, f *harness.Fixture, resource string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?bytestream_url="+url.QueryEscape(resource), nil)
	require.NoError(t, err)
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode, "%s", b)
	return b
}

// Trees are checked after each immutable directory mutation, including a repeat.
func TestDirectoryMutationSurfaces(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	key := f.Web.CreateAPIKey(t, f.GroupID, "second-writer", []string{"CACHE_WRITE"})
	readers := []*harness.Clients{f.Clients(harness.App), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key)}
	instance := "trees/shared"
	var historical []struct {
		d    *repb.Digest
		data []byte
	}
	var roots []struct {
		d   *repb.Digest
		dir *repb.Directory
	}
	checkHistory := func() {
		for _, b := range historical {
			checkBlobSurfaces(t, ctx, f, instance, b.d, b.data)
		}
		for _, root := range roots {
			checkSingleDirectory(t, ctx, f, instance, root.d, root.dir)
		}
	}
	for step, writer := range []*harness.Clients{readers[0], readers[1], readers[0], readers[1]} {
		data := []byte(fmt.Sprintf("tree file revision %d", step/2))
		d, err := writer.Write(ctx, instance, data, false)
		require.NoError(t, err)
		for _, reader := range readers {
			b, err := reader.Read(ctx, instance, d)
			require.NoError(t, err)
			require.Equal(t, data, b)
		}
		historical = append(historical, struct {
			d    *repb.Digest
			data []byte
		}{d, data})
		checkHistory()
		dir := &repb.Directory{Files: []*repb.FileNode{{Name: "file", Digest: d, IsExecutable: true}}}
		root, err := writer.UploadProto(ctx, instance, dir)
		require.NoError(t, err)
		dirBytes, _ := proto.Marshal(dir)
		historical = append(historical, struct {
			d    *repb.Digest
			data []byte
		}{root, dirBytes})
		roots = append(roots, struct {
			d   *repb.Digest
			dir *repb.Directory
		}{root, dir})
		checkHistory()
		checkBlobSurfaces(t, ctx, f, instance, root, dirBytes)
		for _, reader := range readers {
			stream, err := reader.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: root})
			require.NoError(t, err)
			var dirs []*repb.Directory
			for {
				page, err := stream.Recv()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				dirs = append(dirs, page.Directories...)
			}
			require.Len(t, dirs, 1)
			require.True(t, proto.Equal(dir, dirs[0]))
		}
		checkSingleDirectory(t, ctx, f, instance, root, dir)

		require.Equal(t, data, download(t, ctx, f, "bytestream://app:1985/"+harness.ResourceName(instance, d, false)))
		t.Logf("step %d: both keys/endpoints and REv2/UI trees agree immediately", step)
	}
}

func wireBytes(t *testing.T, b []byte, field protowire.Number) []byte {
	t.Helper()
	var result []byte
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if n == field && typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			require.Positive(t, k)
			result = append(result, v...)
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			require.Positive(t, k)
			b = b[k:]
		}
	}
	return result
}
func checkPublicFile(t *testing.T, ctx context.Context, f *harness.Fixture, app *harness.Clients, instance string, d *repb.Digest, want []byte) {
	t.Helper()
	uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, false)
	req := bytesField(nil, 1, []byte(uri))
	stream, err := app.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(&req))
	require.NoError(t, stream.CloseSend())
	var got []byte
	for {
		var res []byte
		err := stream.RecvMsg(&res)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		got = append(got, wireBytes(t, res, 1)...)
	}
	require.Equal(t, want, got)
	if len(want) == 0 {
		return
	}
	req = intField(req, 3, uint64(len(want)))
	var res []byte
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", &req, &res, grpc.ForceCodec(wireCodec{})))
	require.Equal(t, want, wireBytes(t, res, 1))
	rangeStart := len(want) / 2
	rangeReq := bytesField(nil, 1, []byte(uri))
	rangeReq = intField(rangeReq, 2, uint64(rangeStart))
	rangeReq = intField(rangeReq, 3, uint64(len(want)))
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", &rangeReq, &res, grpc.ForceCodec(wireCodec{})))
	require.Equal(t, want[rangeStart:], wireBytes(t, res, 1))
	rangeBody, err := json.Marshal(map[string]any{"uri": uri, "start": rangeStart, "end": len(want)})
	require.NoError(t, err)
	rangeHTTP, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/GetFileRange", bytes.NewReader(rangeBody))
	require.NoError(t, err)
	rangeHTTP.Header.Set("Content-Type", "application/json")
	rangeHTTP.Header.Set("x-buildbuddy-api-key", f.APIKey)
	rangeResponse, err := f.Web.Client.Do(rangeHTTP)
	require.NoError(t, err)
	rangeBytes, err := io.ReadAll(rangeResponse.Body)
	rangeResponse.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, rangeResponse.StatusCode, "%s", rangeBytes)
	var rangeJSON struct {
		Data string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rangeBytes, &rangeJSON))
	decoded, err := base64.StdEncoding.DecodeString(rangeJSON.Data)
	require.NoError(t, err)
	require.Equal(t, want[rangeStart:], decoded)

	body, err := json.Marshal(map[string]any{"uri": uri})
	require.NoError(t, err)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/GetFile", bytes.NewReader(body))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-buildbuddy-api-key", f.APIKey)
	response, err := f.Web.Client.Do(httpReq)
	require.NoError(t, err)
	defer response.Body.Close()
	got, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, 200, response.StatusCode, "%s", got)
	require.Equal(t, want, got)
}

func checkBlobSurfaces(t *testing.T, ctx context.Context, f *harness.Fixture, instance string, d *repb.Digest, want []byte) {
	t.Helper()
	started := time.Now()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		c := f.Clients(component)
		require.Eventually(t, func() bool { b, err := c.Read(ctx, instance, d); return err == nil && bytes.Equal(want, b) }, 10*time.Second, 20*time.Millisecond, "CAS convergence: %s", component)
		for stable := 0; stable < 3; stable++ {
			time.Sleep(20 * time.Millisecond)
			b, err := c.Read(ctx, instance, d)
			require.NoError(t, err)
			require.Equal(t, want, b)
		}

		batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, batch.Responses, 1)
		require.Zero(t, batch.Responses[0].Status.Code)
		require.Equal(t, want, batch.Responses[0].Data)
		require.True(t, proto.Equal(d, batch.Responses[0].Digest))
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Empty(t, missing.MissingBlobDigests)
		if len(want) > 0 {
			require.Equal(t, d.SizeBytes, metadataSize(t, ctx, c, "/cache.service.Cache/GetMetadata", instance, d, 2))
			storedSize := metadataValue(t, ctx, c, "/cache.service.Cache/GetMetadata", instance, d, 2, 2)
			if component == harness.CacheProxy {
				// Proxy read-fill may preserve upstream zstd even when local
				// automatic compression is disabled. Both physical encodings
				// are derived independently from the reference bytes.
				compressed, err := harness.Compress(want)
				require.NoError(t, err)
				require.Contains(t, []int64{d.SizeBytes, int64(len(compressed))}, storedSize)
			} else {
				require.Equal(t, d.SizeBytes, storedSize)
			}
			require.Positive(t, metadataValue(t, ctx, c, "/cache.service.Cache/GetMetadata", instance, d, 2, 3))
			require.Positive(t, metadataValue(t, ctx, c, "/cache.service.Cache/GetMetadata", instance, d, 2, 4))
		}
		req := bytesField(nil, 1, []byte(harness.UploadResourceName(instance, d, false)))
		var res []byte
		require.Equal(t, codes.Unimplemented, status.Code(c.Conn.Invoke(ctx, "/google.bytestream.ByteStream/QueryWriteStatus", &req, &res, grpc.ForceCodec(wireCodec{}))))
		output := filepath.Join(t.TempDir(), "blob")
		cli := exec.CommandContext(ctx, "bb", "download", harness.ResourceName(instance, d, false), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+output)
		cli.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
		log, err := cli.CombinedOutput()
		require.NoError(t, err, "%s", log)
		b, err := os.ReadFile(output)
		require.NoError(t, err)
		require.Equal(t, want, b)
	}
	if len(want) > 0 {
		app := f.Clients(harness.App)
		mdReq := metadataRequest(instance, d, 2)
		var md []byte
		require.NoError(t, app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", &mdReq, &md, grpc.ForceCodec(wireCodec{})))
		framed := uiStream(t, ctx, f, "GetCacheMetadata", mdReq)
		require.Len(t, framed, 1)
		require.Equal(t, d.SizeBytes, wireInt(t, framed[0], 5))
		require.Equal(t, wireInt(t, md, 2), wireInt(t, framed[0], 2))
		require.Equal(t, wireInt(t, md, 4), wireInt(t, framed[0], 4))
		ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "instanceName": instance, "cacheType": 2, "digestFunction": 1}})
		require.NoError(t, err)
		require.Equal(t, strconv.FormatInt(d.SizeBytes, 10), fmt.Sprint(ui["digestSizeBytes"]))
		require.Equal(t, strconv.FormatInt(metadataValue(t, ctx, app, "/cache.service.Cache/GetMetadata", instance, d, 2, 2), 10), fmt.Sprint(ui["storedSizeBytes"]))
		require.Equal(t, strconv.FormatInt(metadataValue(t, ctx, app, "/cache.service.Cache/GetMetadata", instance, d, 2, 4), 10), fmt.Sprint(ui["lastModifyUsec"]))
	}
	require.Equal(t, want, download(t, ctx, f, "bytestream://app:1985/"+harness.ResourceName(instance, d, false)))
	checkPublicFile(t, ctx, f, f.Clients(harness.App), instance, d, want)
	t.Logf("blob %s: all content surfaces agree within %s", d.Hash, time.Since(started))
}

// Use the same length-prefixed protobuf transport as app/service/rpc_service.ts.
func uiStream(t *testing.T, ctx context.Context, f *harness.Fixture, method string, request []byte) [][]byte {
	t.Helper()
	frame := make([]byte, 5+len(request))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(request)))
	copy(frame[5:], request)
	req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(frame))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/proto+prefixed")
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	var pages [][]byte
	for {
		header := make([]byte, 5)
		_, err := io.ReadFull(res.Body, header)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		n := binary.BigEndian.Uint32(header[1:])
		require.Less(t, n, uint32(1<<20))
		payload := make([]byte, n)
		_, err = io.ReadFull(res.Body, payload)
		require.NoError(t, err)
		if header[0]&0x80 != 0 {
			trailers, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(payload, []byte("\r\n")...)))).ReadMIMEHeader()
			require.NoError(t, err)
			require.Equal(t, "0", trailers.Get("Grpc-Status"))
			break
		}
		require.Zero(t, header[0])
		pages = append(pages, payload)
	}
	require.NotEmpty(t, pages)
	return pages
}

func wireInt(t *testing.T, b []byte, field protowire.Number) int64 {
	t.Helper()
	var result int64
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if n == field && typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			require.Positive(t, k)
			result = int64(v)
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			require.Positive(t, k)
			b = b[k:]
		}
	}
	return result
}

func checkSingleDirectory(t *testing.T, ctx context.Context, f *harness.Fixture, instance string, d *repb.Digest, want *repb.Directory) {
	t.Helper()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		stream, err := f.Clients(component).CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: d})
		require.NoError(t, err)
		var dirs []*repb.Directory
		for {
			page, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			dirs = append(dirs, page.Directories...)
		}
		require.Len(t, dirs, 1)
		require.True(t, proto.Equal(want, dirs[0]))
	}
	db, _ := proto.Marshal(d)
	req := bytesField(nil, 3, db)
	req = bytesField(req, 2, []byte(instance))
	req = intField(req, 6, 1)
	pages := uiStream(t, ctx, f, "GetTree", req)
	require.Len(t, pages, 1)
	got := &repb.Directory{}
	require.NoError(t, proto.Unmarshal(wireBytes(t, pages[0], 2), got))
	require.True(t, proto.Equal(want, got))
	stream, err := f.Clients(harness.App).Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(&req))
	require.NoError(t, stream.CloseSend())
	var page []byte
	require.NoError(t, stream.RecvMsg(&page))
	got = &repb.Directory{}
	require.NoError(t, proto.Unmarshal(wireBytes(t, page, 2), got))
	require.True(t, proto.Equal(want, got))
	require.Equal(t, io.EOF, stream.RecvMsg(&page))
	total := d.SizeBytes
	for _, file := range want.Files {
		total += file.Digest.SizeBytes
	}
	sizeReq := bytesField(nil, 2, db)
	sizeReq = bytesField(sizeReq, 3, []byte(instance))
	sizeReq = intField(sizeReq, 4, 1)
	var sizes []byte
	require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", &sizeReq, &sizes, grpc.ForceCodec(wireCodec{})))
	for _, projection := range append([][]byte{sizes}, uiStream(t, ctx, f, "GetTreeDirectorySizes", sizeReq)...) {
		item := wireBytes(t, projection, 2)
		require.Equal(t, d.Hash, string(wireBytes(t, item, 1)))
		require.Equal(t, total, wireInt(t, item, 2))
		require.Equal(t, int64(len(want.Files)), wireInt(t, item, 3))
	}
	ui, err := f.Web.RPC("GetTreeDirectorySizes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "rootDigest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "instanceName": instance, "digestFunction": 1})
	require.NoError(t, err)
	entries := ui["sizes"].([]any)
	require.Len(t, entries, 1)
	entry := entries[0].(map[string]any)
	require.Equal(t, d.Hash, entry["digest"])
	require.Equal(t, strconv.FormatInt(total, 10), fmt.Sprint(entry["totalSize"]))
	require.Equal(t, strconv.Itoa(len(want.Files)), fmt.Sprint(entry["childCount"]))
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		out := t.TempDir()
		cli := exec.CommandContext(ctx, "bb", "download", harness.ResourceName(instance, d, false), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_directory="+out)
		cli.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
		log, err := cli.CombinedOutput()
		require.NoError(t, err, "%s", log)
		for _, file := range want.Files {
			expected, err := f.Clients(component).Read(ctx, instance, file.Digest)
			require.NoError(t, err)
			actual, err := os.ReadFile(filepath.Join(out, file.Name))
			require.NoError(t, err)
			require.Equal(t, expected, actual)
			info, err := os.Stat(filepath.Join(out, file.Name))
			require.NoError(t, err)
			require.Equal(t, file.IsExecutable, info.Mode()&0111 != 0)
		}
	}

}

// Observe convergence first, then assert every representation and stability.
// No mutation response participates in the expected model.
func checkActionSurfaces(t *testing.T, ctx context.Context, f *harness.Fixture, instance string, action *repb.Digest, want *repb.ActionResult) {
	t.Helper()
	started := time.Now()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		c := f.Clients(component)
		require.Eventually(t, func() bool {
			got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
			if want == nil {
				return status.Code(err) == codes.NotFound
			}
			return err == nil && proto.Equal(want, got)
		}, 10*time.Second, 20*time.Millisecond, "AC convergence: %s", component)
		for projection := 0; projection < 8; projection++ {
			req := &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action, InlineStdout: projection&2 != 0, InlineStderr: projection&4 != 0}
			if want != nil && projection&1 != 0 {
				for _, file := range want.OutputFiles {
					req.InlineOutputFiles = append(req.InlineOutputFiles, file.Path)
				}
			}
			got, err := c.AC.GetActionResult(ctx, req)
			if want == nil {
				require.Equal(t, codes.NotFound, status.Code(err))
				continue
			}
			require.NoError(t, err)
			expected := proto.Clone(want).(*repb.ActionResult)
			if projection&1 != 0 {
				for _, file := range expected.OutputFiles {
					data, err := c.Read(ctx, instance, file.Digest)
					require.NoError(t, err)
					file.Contents = data
				}
			}
			require.True(t, proto.Equal(expected, got), "AC %s projection %d: want %s got %s", component, projection, expected, got)
		}
	}
	observed := time.Since(started)
	resource := fmt.Sprintf("actioncache://app:1985/%s/blobs/ac/%s/%d", instance, action.Hash, action.SizeBytes)
	if want == nil {
		req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?bytestream_url="+url.QueryEscape(resource), nil)
		require.NoError(t, err)
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		res, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		require.Equal(t, http.StatusNotFound, res.StatusCode)
		for _, method := range []string{"/cache.service.Cache/GetMetadata", "/buildbuddy.service.BuildBuddyService/GetCacheMetadata"} {
			req := metadataRequest(instance, action, 1)
			var res []byte
			require.Equal(t, codes.NotFound, status.Code(f.Clients(harness.App).Conn.Invoke(ctx, method, &req, &res, grpc.ForceCodec(wireCodec{}))))
		}
	} else {
		got := &repb.ActionResult{}
		require.NoError(t, proto.Unmarshal(download(t, ctx, f, resource), got))
		require.True(t, proto.Equal(want, got))
		req := metadataRequest(instance, action, 1)
		var md []byte
		require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", &req, &md, grpc.ForceCodec(wireCodec{})))
		pages := uiStream(t, ctx, f, "GetCacheMetadata", req)
		require.Len(t, pages, 1)
		require.Equal(t, action.SizeBytes, wireInt(t, pages[0], 5))
		require.Equal(t, wireInt(t, md, 2), wireInt(t, pages[0], 2))
		require.Equal(t, wireInt(t, md, 4), wireInt(t, pages[0], 4))
		require.Positive(t, wireInt(t, md, 3))
		require.Positive(t, wireInt(t, md, 4))
	}
	for stable := 0; stable < 3; stable++ {
		time.Sleep(20 * time.Millisecond)
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			got, err := f.Clients(component).AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
			if want == nil {
				require.Equal(t, codes.NotFound, status.Code(err))
			} else {
				require.NoError(t, err)
				require.True(t, proto.Equal(want, got))
			}
		}
	}
	t.Logf("AC convergence observed within %s; stable for three separated reads", observed)
}

func orderedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
