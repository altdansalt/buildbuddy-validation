package metadata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Wire-only codec for the released private metadata protos, which are not a
// scaffold dependency. Field numbers are from v2.314.0 proto/{cache,resource}.
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
func metadata(ctx context.Context, c *harness.Clients, method, instance string, d *repb.Digest, kind int) (map[int]int64, error) {
	db, _ := proto.Marshal(d)
	rn := bytesField(nil, 1, db)
	rn = bytesField(rn, 2, []byte(instance))
	rn = intField(rn, 4, uint64(kind))
	rn = intField(rn, 5, 1)
	req := bytesField(nil, 2, rn)
	var resp []byte
	if err := c.Conn.Invoke(ctx, method, &req, &resp, grpc.ForceCodec(wireCodec{})); err != nil {
		return nil, err
	}
	fields := map[int]int64{}
	for len(resp) > 0 {
		n, typ, k := protowire.ConsumeTag(resp)
		if k < 0 {
			return nil, fmt.Errorf("invalid metadata tag")
		}
		resp = resp[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(resp)
			if k < 0 {
				return nil, fmt.Errorf("invalid metadata value")
			}
			fields[int(n)] = int64(v)
			resp = resp[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, resp)
			if k < 0 {
				return nil, fmt.Errorf("invalid metadata field")
			}
			resp = resp[k:]
		}
	}
	return fields, nil
}
func number(v any) int64 { n, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64); return n }

func TestMetadataStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	key2 := f.Web.CreateAPIKey(t, f.GroupID, "second-model-client", []string{"CAS_WRITE", "CACHE_WRITE"})
	clients := []*harness.Clients{f.Clients(harness.App), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key2)}
	const instance = "metadata-model"
	checkMetadata := func(d *repb.Digest, kind int) {
		t.Helper()
		// Metadata itself does not populate the proxy. Call only after content reads.
		metadataReaders := clients
		if kind == 1 {
			metadataReaders = clients[:1]
		} // Proxy AC keys are request hashes, not action digests.
		for _, c := range metadataReaders {
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				m, err := metadata(ctx, c, "/cache.service.Cache/GetMetadata", instance, d, kind)
				require.NoError(ct, err)
				if err != nil {
					return
				}
				require.Equal(ct, d.SizeBytes, m[5])
				require.Greater(ct, m[2], int64(0))
				require.Greater(ct, m[3], int64(0))
				require.Greater(ct, m[4], int64(0))
			}, 10*time.Second, 100*time.Millisecond)
		}
		m, err := metadata(ctx, clients[0], "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", instance, d, kind)
		require.NoError(t, err)
		require.Equal(t, d.SizeBytes, m[5])
		ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "instanceName": instance, "cacheType": kind, "digestFunction": 1}})
		require.NoError(t, err)
		require.Equal(t, d.SizeBytes, number(ui["digestSizeBytes"]))
		require.Greater(t, number(ui["storedSizeBytes"]), int64(0))
	}
	// CAS reference model: digest -> immutable bytes. Cover each mutation through
	// both endpoints, fresh creation and successful idempotent repetition.
	for endpoint, c := range clients {
		for _, op := range []string{"batch", "write", "zstd"} {
			t.Run(fmt.Sprintf("CAS/%d/%s", endpoint, op), func(t *testing.T) {
				data := []byte(fmt.Sprintf("metadata conformance endpoint=%d operation=%s\n", endpoint, op))
				d := harness.Digest(data)
				for _, reader := range clients {
					_, err := metadata(ctx, reader, "/cache.service.Cache/GetMetadata", instance, d, 2)
					require.Equal(t, codes.NotFound, status.Code(err))
				}
				for repeat := 0; repeat < 2; repeat++ {
					var err error
					if op == "batch" {
						_, err = c.Upload(ctx, instance, data)
					} else {
						_, err = c.Write(ctx, instance, data, op == "zstd")
					}
					require.NoError(t, err)
					m, metaErr := metadata(ctx, clients[1], "/cache.service.Cache/GetMetadata", instance, d, 2)
					t.Logf("proxy metadata before content read: fields=%v status=%s", m, status.Code(metaErr))
					checkContent(t, ctx, f, clients, instance, d, data)
					for _, reader := range clients {
						got, err := reader.Read(ctx, instance, d)
						require.NoError(t, err)
						require.Equal(t, data, got)
						batch, err := reader.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
						require.NoError(t, err)
						require.Len(t, batch.Responses, 1)
						require.Zero(t, batch.Responses[0].GetStatus().GetCode())
						require.Equal(t, data, batch.Responses[0].Data)
						missing, err := reader.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
						require.NoError(t, err)
						require.Empty(t, missing.MissingBlobDigests)
					}
					checkMetadata(d, 2)
					t.Run("bb-download", func(t *testing.T) {
						bb, err := exec.LookPath("bb")
						if err != nil {
							t.Fatal("required bb CLI is absent")
						}
						cmd := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(harness.CacheProxy).GRPC, "--api_key="+f.APIKey, harness.ResourceName(instance, d, false))
						var stderr bytes.Buffer
						cmd.Stderr = &stderr
						got, err := cmd.Output()
						require.NoError(t, err, stderr.String())
						require.Equal(t, data, got)
					})
				}
			})
		}
	}
	// SpliceBlob stores a manifest, not a materialized blob metadata record.
	// SplitBlob exposes that representation; content reads reconstruct bytes.
	for endpoint, c := range clients {
		t.Run(fmt.Sprintf("splice/%d", endpoint), func(t *testing.T) {
			left := bytes.Repeat([]byte{byte(65 + endpoint)}, 3<<20)
			right := bytes.Repeat([]byte("r"), 2<<20)
			ld, err := c.Upload(ctx, instance, left)
			require.NoError(t, err)
			rd, err := c.Upload(ctx, instance, right)
			require.NoError(t, err)
			data := append(append([]byte(nil), left...), right...)
			d := harness.Digest(data)
			for repeat := 0; repeat < 2; repeat++ {
				_, err := c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{InstanceName: instance, BlobDigest: d, ChunkDigests: []*repb.Digest{ld, rd}, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				checkContent(t, ctx, f, clients, instance, d, data)
				for _, reader := range clients {
					split, err := reader.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{InstanceName: instance, BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
					require.NoError(t, err)
					require.Len(t, split.ChunkDigests, 2)
					require.True(t, proto.Equal(ld, split.ChunkDigests[0]))
					require.True(t, proto.Equal(rd, split.ChunkDigests[1]))
					got, err := reader.Read(ctx, instance, d)
					require.NoError(t, err)
					require.Equal(t, data, got)
				}
			}
		})
	}

	// AC reference model: action digest -> latest ActionResult. Keep the same key
	// for replacements, and use different serialized sizes to expose stale state.
	for endpoint := range clients {
		t.Run(fmt.Sprintf("AC/%d", endpoint), func(t *testing.T) {
			d := harness.Digest([]byte(fmt.Sprintf("action-key-%d", endpoint)))
			for _, reader := range clients {
				_, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d})
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			output := []byte(fmt.Sprintf("output-%d", endpoint))
			od, err := clients[endpoint].Upload(ctx, instance, output)
			require.NoError(t, err)
			checkContent(t, ctx, f, clients, instance, od, output)
			tree := &repb.Tree{Root: &repb.Directory{Files: []*repb.FileNode{{Name: "artifact", Digest: od}}}}
			tb, td, err := harness.MarshalDigest(tree)
			require.NoError(t, err)
			_, err = clients[endpoint].Upload(ctx, instance, tb)
			require.NoError(t, err)
			checkContent(t, ctx, f, clients, instance, td, tb)
			var previousModify int64
			for step, stdout := range []string{"first", "a longer replacement result", "third", "third"} {
				model := &repb.ActionResult{StdoutRaw: []byte(stdout), StderrRaw: []byte("stderr-" + stdout), ExitCode: int32(min(step, 2)), ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: "model-worker"}, OutputFiles: []*repb.OutputFile{{Path: "artifact", Digest: od, IsExecutable: true}}, OutputDirectories: []*repb.OutputDirectory{{Path: "dir", TreeDigest: td}}, OutputFileSymlinks: []*repb.OutputSymlink{{Path: "link", Target: "artifact"}}}
				start := time.Now()
				_, err := clients[(endpoint+step)%2].AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, ActionResult: model})
				require.NoError(t, err)
				for _, reader := range clients {
					require.EventuallyWithT(t, func(ct *assert.CollectT) {
						got, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, InlineStdout: true})
						require.NoError(ct, err)
						if err == nil {
							require.True(ct, proto.Equal(model, got), "got %v want %v", got, model)
						}
					}, 10*time.Second, 100*time.Millisecond)
				}
				checkMetadata(d, 1)
				db, _ := proto.Marshal(d)
				rn := bytesField(nil, 1, db)
				rn = bytesField(rn, 2, []byte(instance))
				rn = intField(rn, 4, 1)
				rn = intField(rn, 5, 1)
				frames := httpRPC(t, ctx, f, "GetCacheMetadata", bytesField(nil, 2, rn))
				require.Len(t, frames, 1)
				fields := wireNumbers(t, frames[0])
				require.Equal(t, int64(proto.Size(model)), fields[2])
				require.Equal(t, d.SizeBytes, fields[5])
				require.Greater(t, fields[4], previousModify)
				for _, method := range []string{"/cache.service.Cache/GetMetadata", "/buildbuddy.service.BuildBuddyService/GetCacheMetadata"} {
					m, err := metadata(ctx, clients[0], method, instance, d, 1)
					require.NoError(t, err)
					require.Equal(t, int64(proto.Size(model)), m[2])
					require.Greater(t, m[4], previousModify)
				}
				ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": 1, "digestFunction": 1}})
				require.NoError(t, err)
				require.Equal(t, int64(proto.Size(model)), number(ui["storedSizeBytes"]))
				m, err := metadata(ctx, clients[0], "/cache.service.Cache/GetMetadata", instance, d, 1)
				require.NoError(t, err)
				previousModify = m[4]
				t.Logf("AC converged in %s", time.Since(start))
				for stable := 0; stable < 3; stable++ {
					for _, reader := range clients {
						for _, inline := range []bool{false, true} {
							got, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, InlineStdout: inline, InlineStderr: inline})
							require.NoError(t, err)
							require.True(t, proto.Equal(model, got), "AC full result mismatch: %v", got)
						}
					}
					for _, reader := range clients {
						got, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, InlineOutputFiles: []string{"artifact"}})
						require.NoError(t, err)
						inlineModel := proto.Clone(model).(*repb.ActionResult)
						inlineModel.OutputFiles[0].Contents = output
						require.True(t, proto.Equal(inlineModel, got), "inline file result: %v", got)
					}
					for _, inline := range []bool{false, true} {
						request := &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, InlineStdout: inline, InlineStderr: inline}
						rb, err := proto.Marshal(request)
						require.NoError(t, err)
						key := harness.Digest(append(rb, []byte("actioncache-170401")...))
						resultBytes, resultDigest, err := harness.MarshalDigest(model)
						require.NoError(t, err)
						_ = resultBytes
						db, _ := proto.Marshal(resultDigest)
						pointer := bytesField(nil, 1, db)
						pointer = bytesField(pointer, 2, []byte(instance))
						pointer = intField(pointer, 4, 2)
						pointer = intField(pointer, 5, 1)
						md, err := metadata(ctx, clients[1], "/cache.service.Cache/GetMetadata", instance, key, 1)
						require.NoError(t, err)
						storedPointer := pointer
						if key.SizeBytes >= 100 {
							storedPointer, err = harness.Compress(pointer)
							require.NoError(t, err)
						}
						require.Equal(t, int64(len(storedPointer)), md[2])
						require.Equal(t, key.SizeBytes, md[5])
						md, err = metadata(ctx, clients[1], "/cache.service.Cache/GetMetadata", instance, resultDigest, 2)
						require.NoError(t, err)
						require.Equal(t, resultDigest.SizeBytes, md[5])
					}
					got := &repb.ActionResult{}
					require.NoError(t, proto.Unmarshal(downloadContent(t, ctx, f, fmt.Sprintf("actioncache://app:1985/%s/blobs/ac/%s/%d", instance, d.Hash, d.SizeBytes)), got))
					require.True(t, proto.Equal(model, got))
					m, err := metadata(ctx, clients[0], "/cache.service.Cache/GetMetadata", instance, d, 1)
					require.NoError(t, err)
					require.Equal(t, int64(proto.Size(model)), m[2])
					time.Sleep(100 * time.Millisecond)
				}
			}
		})
	}
}

// Audit the UI content surface omitted by the original metadata-only checks.
func TestAuditMissingSurfaces(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	key2 := f.Web.CreateAPIKey(t, f.GroupID, "second-model-client", []string{"CAS_WRITE", "CACHE_WRITE"})
	clients := []*harness.Clients{f.Clients(harness.App), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key2)}
	const instance = "audit-missing"
	download := func(t *testing.T, resource string) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?bytestream_url="+url.QueryEscape(resource), nil)
		require.NoError(t, err)
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		resp, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, string(data))
		return data
	}
	for endpoint, c := range clients {
		for _, op := range []string{"batch", "write", "zstd"} {
			t.Run(fmt.Sprintf("HTTP-CAS/%d/%s", endpoint, op), func(t *testing.T) {
				data := []byte(fmt.Sprintf("audit content %d %s", endpoint, op))
				d := harness.Digest(data)
				var err error
				if op == "batch" {
					_, err = c.Upload(ctx, instance, data)
				} else {
					_, err = c.Write(ctx, instance, data, op == "zstd")
				}
				require.NoError(t, err)
				require.Equal(t, data, download(t, "bytestream://app:1985/"+harness.ResourceName(instance, d, false)))
			})
		}
		t.Run(fmt.Sprintf("HTTP-AC/%d", endpoint), func(t *testing.T) {
			d := harness.Digest([]byte(fmt.Sprintf("audit action %d", endpoint)))
			for step, text := range []string{"first", "replacement"} {
				model := &repb.ActionResult{StdoutRaw: []byte(text), StderrRaw: []byte("stderr-" + text), ExitCode: 7}
				_, err := clients[(endpoint+step)%2].AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, ActionResult: model})
				require.NoError(t, err)
				data := download(t, fmt.Sprintf("actioncache://app:1985/%s/blobs/ac/%s/%d", instance, d.Hash, d.SizeBytes))
				got := &repb.ActionResult{}
				require.NoError(t, proto.Unmarshal(data, got))
				require.Equal(t, model.StdoutRaw, got.StdoutRaw)
				require.Equal(t, model.StderrRaw, got.StderrRaw)
				require.Equal(t, model.ExitCode, got.ExitCode)
			}
		})
		t.Run(fmt.Sprintf("splice-extra/%d", endpoint), func(t *testing.T) {
			left := bytes.Repeat([]byte{byte(77 + endpoint)}, 3<<20)
			right := bytes.Repeat([]byte("s"), 2<<20)
			ld, err := c.Upload(ctx, instance, left)
			require.NoError(t, err)
			rd, err := c.Upload(ctx, instance, right)
			require.NoError(t, err)
			data := append(append([]byte(nil), left...), right...)
			d := harness.Digest(data)
			_, err = c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{InstanceName: instance, BlobDigest: d, ChunkDigests: []*repb.Digest{ld, rd}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			// Check existence BEFORE ByteStream can populate local caches.
			// A 5 MiB whole blob exceeds the default BatchReadBlobs limit (4,000,000).
			for i, reader := range clients {
				missing, err := reader.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				assert.Empty(t, missing.MissingBlobDigests, "reader %d", i)
			}
			require.Equal(t, data, download(t, "bytestream://app:1985/"+harness.ResourceName(instance, d, false)))
		})
	}
}

func downloadContent(t *testing.T, ctx context.Context, f *harness.Fixture, resource string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?bytestream_url="+url.QueryEscape(resource), nil)
	require.NoError(t, err)
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	resp, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, string(data))
	return data
}
func checkContent(t *testing.T, ctx context.Context, f *harness.Fixture, clients []*harness.Clients, instance string, d *repb.Digest, data []byte) {
	t.Helper()
	start := time.Now()
	// Existence is checked before any content read can fill the proxy cache.
	for _, c := range clients {
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
			require.NoError(ct, err)
			if err == nil {
				require.Empty(ct, missing.MissingBlobDigests)
			}
		}, 10*time.Second, 100*time.Millisecond)
	}
	t.Logf("existence converged in %s", time.Since(start))
	for stable := 0; stable < 3; stable++ {
		for _, c := range clients {
			got, err := c.Read(ctx, instance, d)
			require.NoError(t, err)
			require.Equal(t, data, got)
			stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, true)})
			require.NoError(t, err)
			var compressed []byte
			for {
				r, err := stream.Recv()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				compressed = append(compressed, r.Data...)
			}
			got, err = harness.Decompress(compressed)
			require.NoError(t, err)
			require.Equal(t, data, got)
			if len(data) < 4000000 {
				batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
				require.NoError(t, err)
				require.Len(t, batch.Responses, 1)
				r := batch.Responses[0]
				require.Zero(t, r.GetStatus().GetCode())
				got = r.Data
				if r.Compressor == repb.Compressor_ZSTD {
					got, err = harness.Decompress(got)
					require.NoError(t, err)
				}
				require.Equal(t, data, got)
			}
		}
		require.Equal(t, data, downloadContent(t, ctx, f, "bytestream://app:1985/"+harness.ResourceName(instance, d, false)))
		if len(data) < 4000000 {
			for _, reader := range clients {
				md, err := metadata(ctx, reader, "/cache.service.Cache/GetMetadata", instance, d, 2)
				require.NoError(t, err)
				require.Equal(t, d.SizeBytes, md[5])
				require.Positive(t, md[2])
				require.Positive(t, md[3])
				require.Positive(t, md[4])
			}
			appMD, err := metadata(ctx, clients[0], "/cache.service.Cache/GetMetadata", instance, d, 2)
			require.NoError(t, err)
			uiMD, err := metadata(ctx, clients[0], "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", instance, d, 2)
			require.NoError(t, err)
			require.Equal(t, appMD[2], uiMD[2])
			require.Equal(t, appMD[4], uiMD[4])
			require.Equal(t, d.SizeBytes, uiMD[5])
			ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": 2, "digestFunction": 1}})
			require.NoError(t, err)
			require.Equal(t, d.SizeBytes, number(ui["digestSizeBytes"]))
			require.Equal(t, appMD[2], number(ui["storedSizeBytes"]))
			db, _ := proto.Marshal(d)
			rn := bytesField(nil, 1, db)
			rn = bytesField(rn, 2, []byte(instance))
			rn = intField(rn, 4, 2)
			rn = intField(rn, 5, 1)
			frames := httpRPC(t, ctx, f, "GetCacheMetadata", bytesField(nil, 2, rn))
			require.Len(t, frames, 1)
			md := wireNumbers(t, frames[0])
			require.Equal(t, appMD[2], md[2])
			require.Equal(t, d.SizeBytes, md[5])
		}

		if stable == 0 && len(data) < 4000000 {
			bb, err := exec.LookPath("bb")
			require.NoError(t, err)
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				cmd := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, harness.ResourceName(instance, d, false))
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				got, err := cmd.Output()
				require.NoError(t, err, stderr.String())
				require.Equal(t, data, got)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestDirectoryProjections(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	key := f.Web.CreateAPIKey(t, f.GroupID, "directory-client", []string{"CAS_WRITE", "CACHE_WRITE"})
	clients := []*harness.Clients{f.Clients(harness.App), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key)}
	const instance = "directory-model"
	for step, op := range []string{"batch", "write", "zstd", "batch"} {
		data := []byte(fmt.Sprintf("file-%d", step))
		fd, err := clients[step%2].Upload(ctx, instance, data)
		require.NoError(t, err)
		checkContent(t, ctx, f, clients, instance, fd, data)
		child := &repb.Directory{Files: []*repb.FileNode{{Name: "output.txt", Digest: fd, IsExecutable: step%2 == 0}}}
		cb, cd, err := harness.MarshalDigest(child)
		require.NoError(t, err)
		_, err = clients[(step+1)%2].Upload(ctx, instance, cb)
		require.NoError(t, err)
		checkContent(t, ctx, f, clients, instance, cd, cb)
		dir := &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "nested", Digest: cd}}}
		raw, d, err := harness.MarshalDigest(dir)
		require.NoError(t, err)
		if op == "batch" {
			_, err = clients[step%2].Upload(ctx, instance, raw)
		} else {
			_, err = clients[step%2].Write(ctx, instance, raw, op == "zstd")
		}
		require.NoError(t, err)
		checkContent(t, ctx, f, clients, instance, d, raw)
		for stable := 0; stable < 3; stable++ {
			for _, c := range clients {
				stream, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: d})
				require.NoError(t, err)
				var dirs []*repb.Directory
				for {
					r, err := stream.Recv()
					if err == io.EOF {
						break
					}
					require.NoError(t, err)
					dirs = append(dirs, r.Directories...)
				}
				checkDirectories(t, dirs, dir, child)
			}
			db, _ := proto.Marshal(d)
			req := bytesField(nil, 2, []byte(instance))
			req = bytesField(req, 3, db)
			req = intField(req, 6, 1)
			stream, err := clients[0].Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree", grpc.ForceCodec(wireCodec{}))
			require.NoError(t, err)
			require.NoError(t, stream.SendMsg(&req))
			require.NoError(t, stream.CloseSend())
			var grpcDirs []*repb.Directory
			for {
				var resp []byte
				err := stream.RecvMsg(&resp)
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				for len(resp) > 0 {
					n, typ, k := protowire.ConsumeTag(resp)
					require.Positive(t, k)
					resp = resp[k:]
					if n == 2 && typ == protowire.BytesType {
						b, k := protowire.ConsumeBytes(resp)
						require.Positive(t, k)
						got := &repb.Directory{}
						require.NoError(t, proto.Unmarshal(b, got))
						grpcDirs = append(grpcDirs, got)
						resp = resp[k:]
					} else {
						k := protowire.ConsumeFieldValue(n, typ, resp)
						require.Positive(t, k)
						resp = resp[k:]
					}
				}
			}
			checkDirectories(t, grpcDirs, dir, child)
			frames := httpRPC(t, ctx, f, "GetTree", req)
			var httpDirs []*repb.Directory
			for _, frame := range frames {
				for len(frame) > 0 {
					n, typ, k := protowire.ConsumeTag(frame)
					require.Positive(t, k)
					frame = frame[k:]
					if n == 2 && typ == protowire.BytesType {
						b, k := protowire.ConsumeBytes(frame)
						require.Positive(t, k)
						got := &repb.Directory{}
						require.NoError(t, proto.Unmarshal(b, got))
						httpDirs = append(httpDirs, got)
						frame = frame[k:]
					} else {
						k := protowire.ConsumeFieldValue(n, typ, frame)
						require.Positive(t, k)
						frame = frame[k:]
					}
				}
			}
			checkDirectories(t, httpDirs, dir, child)
			ui, err := f.Web.RPC("GetTreeDirectorySizes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "rootDigest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "digestFunction": 1})
			require.NoError(t, err)
			sizes, ok := ui["sizes"].([]any)
			require.True(t, ok)
			require.Len(t, sizes, 2)
			for _, entry := range sizes {
				size := entry.(map[string]any)
				if size["digest"] == d.Hash {
					require.Equal(t, d.SizeBytes+cd.SizeBytes+fd.SizeBytes, number(size["totalSize"]))
					require.Equal(t, int64(2), number(size["childCount"]))
				} else {
					require.Equal(t, cd.Hash, size["digest"])
					require.Equal(t, cd.SizeBytes+fd.SizeBytes, number(size["totalSize"]))
					require.Equal(t, int64(1), number(size["childCount"]))
				}
			}
			sizesReq := bytesField(nil, 2, db)
			sizesReq = bytesField(sizesReq, 3, []byte(instance))
			sizesReq = intField(sizesReq, 4, 1)
			var sizeResponse []byte
			require.NoError(t, clients[0].Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", &sizesReq, &sizeResponse, grpc.ForceCodec(wireCodec{})))
			checkSizes(t, sizeResponse, map[string][2]int64{d.Hash: {d.SizeBytes + cd.SizeBytes + fd.SizeBytes, 2}, cd.Hash: {cd.SizeBytes + fd.SizeBytes, 1}})
			sizeFrames := httpRPC(t, ctx, f, "GetTreeDirectorySizes", sizesReq)
			require.Len(t, sizeFrames, 1)
			checkSizes(t, sizeFrames[0], map[string][2]int64{d.Hash: {d.SizeBytes + cd.SizeBytes + fd.SizeBytes, 2}, cd.Hash: {cd.SizeBytes + fd.SizeBytes, 1}})
			bb, err := exec.LookPath("bb")
			require.NoError(t, err)
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				view := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--type=Directory", harness.ResourceName(instance, d, false))
				viewBytes, err := view.Output()
				require.NoError(t, err)
				viewDir := &repb.Directory{}
				require.NoError(t, protojson.Unmarshal(viewBytes, viewDir))
				require.True(t, proto.Equal(dir, viewDir))
				out := t.TempDir()
				cmd := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_directory="+out, harness.ResourceName(instance, d, false))
				log, err := cmd.CombinedOutput()
				require.NoError(t, err, string(log))
				got, err := os.ReadFile(filepath.Join(out, "nested", "output.txt"))
				require.NoError(t, err)
				require.Equal(t, data, got)
			}
			time.Sleep(100 * time.Millisecond)
		}

	}
}

// Exercise the actual frontend transport, including terminal gRPC-web status.
func httpRPC(t *testing.T, ctx context.Context, f *harness.Fixture, method string, message []byte) [][]byte {
	t.Helper()
	framed := make([]byte, 5)
	binary.BigEndian.PutUint32(framed[1:], uint32(len(message)))
	framed = append(framed, message...)
	req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(framed))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/proto+prefixed")
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	resp, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var frames [][]byte
	terminal := resp.Header.Get("grpc-status") == "0"
	for len(raw) > 0 {
		require.GreaterOrEqual(t, len(raw), 5)
		flags := raw[0]
		n := int(binary.BigEndian.Uint32(raw[1:5]))
		require.GreaterOrEqual(t, len(raw), 5+n)
		body := raw[5 : 5+n]
		raw = raw[5+n:]
		if flags == 0x80 {
			require.Contains(t, strings.ToLower(string(body)), "grpc-status: 0")
			terminal = true
		} else {
			require.Zero(t, flags)
			frames = append(frames, body)
		}
	}
	require.True(t, terminal, "missing terminal success status")
	return frames
}

func checkDirectories(t *testing.T, got []*repb.Directory, want ...*repb.Directory) {
	t.Helper()
	require.Len(t, got, len(want))
	seen := map[string]bool{}
	for _, dir := range got {
		raw, d, err := harness.MarshalDigest(dir)
		require.NoError(t, err)
		_ = raw
		require.False(t, seen[d.Hash], "duplicate directory")
		seen[d.Hash] = true
	}
	for _, dir := range want {
		_, d, err := harness.MarshalDigest(dir)
		require.NoError(t, err)
		require.True(t, seen[d.Hash], "missing directory %s", d.Hash)
	}
}

func wireNumbers(t *testing.T, raw []byte) map[int]int64 {
	t.Helper()
	fields := map[int]int64{}
	for len(raw) > 0 {
		n, typ, k := protowire.ConsumeTag(raw)
		require.Positive(t, k)
		raw = raw[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(raw)
			require.Positive(t, k)
			fields[int(n)] = int64(v)
			raw = raw[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, raw)
			require.Positive(t, k)
			raw = raw[k:]
		}
	}
	return fields
}

func checkSizes(t *testing.T, raw []byte, want map[string][2]int64) {
	t.Helper()
	got := map[string][2]int64{}
	for len(raw) > 0 {
		n, typ, k := protowire.ConsumeTag(raw)
		require.Positive(t, k)
		raw = raw[k:]
		if n == 2 && typ == protowire.BytesType {
			entry, k := protowire.ConsumeBytes(raw)
			require.Positive(t, k)
			raw = raw[k:]
			fields := wireNumbers(t, entry)
			var hash string
			for len(entry) > 0 {
				n, typ, k := protowire.ConsumeTag(entry)
				require.Positive(t, k)
				entry = entry[k:]
				if n == 1 && typ == protowire.BytesType {
					b, k := protowire.ConsumeBytes(entry)
					require.Positive(t, k)
					hash = string(b)
					entry = entry[k:]
				} else {
					k := protowire.ConsumeFieldValue(n, typ, entry)
					require.Positive(t, k)
					entry = entry[k:]
				}
			}
			require.NotEmpty(t, hash)
			_, duplicate := got[hash]
			require.False(t, duplicate)
			got[hash] = [2]int64{fields[2], fields[3]}
		} else {
			k := protowire.ConsumeFieldValue(n, typ, raw)
			require.Positive(t, k)
			raw = raw[k:]
		}
	}
	require.Equal(t, want, got)
}
