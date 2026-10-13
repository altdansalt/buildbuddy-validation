package public_cache_delete_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Wire codec avoids importing unreleased/generated BuildBuddy bindings. Field
// numbers are from the v2.314.0 protos documented in COVERAGE.md.
type wire []byte
type codec struct{}

func (codec) Name() string                    { return "proto" }
func (codec) Marshal(v any) ([]byte, error)   { return []byte(*v.(*wire)), nil }
func (codec) Unmarshal(b []byte, v any) error { *v.(*wire) = append(wire(nil), b...); return nil }
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func integer(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func data(b []byte) []byte {
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			if n == 1 {
				return v
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			b = b[k:]
		}
	}
	return nil
}

func TestDeleteStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true", "--cache.check_client_action_result_digests=true"}}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	invocationID := "b7b751bd-2619-40ba-ad97-201e89296c22"
	publishInvocation(t, ctx, c, invocationID)
	invoke := func(method string, b []byte) (wire, error) {
		req := wire(b)
		var res wire
		err := retryUnavailable(ctx, func() error { return c.Conn.Invoke(ctx, method, &req, &res, grpc.ForceCodec(codec{})) })
		return res, err
	}
	httpCall := func(t *testing.T, method string, req any) (int, []byte) {
		b, err := json.Marshal(req)
		require.NoError(t, err)
		r, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(b))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("x-buildbuddy-api-key", f.APIKey)
		resp, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, out
	}
	checkFile := func(t *testing.T, uri string, present bool, expected []byte) {
		stream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile", grpc.ForceCodec(codec{}))
		require.NoError(t, err)
		req := wire(field(1, []byte(uri)))
		require.NoError(t, stream.SendMsg(&req))
		require.NoError(t, stream.CloseSend())
		var all []byte
		for {
			var chunk wire
			err = stream.RecvMsg(&chunk)
			if err != nil {
				break
			}
			all = append(all, data(chunk)...)
		}
		if present {
			require.Equal(t, io.EOF, err)
			require.Equal(t, expected, all)
		} else {
			// PooledByteStreamClient intentionally wraps failed reads in Unavailable.
			require.Equal(t, codes.Unavailable, status.Code(err))
			require.Empty(t, all)
		}
		code, out := httpCall(t, "GetFile", map[string]any{"uri": uri})
		if present {
			require.Equal(t, 200, code)
			require.Equal(t, expected, out)
		} else {
			require.Equal(t, 404, code)
		}
		for _, route := range []string{"download", "view"} {
			r, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/"+route+"?invocation_id="+invocationID+"&bytestream_url="+url.QueryEscape(uri), nil)
			require.NoError(t, err)
			r.Header.Set("x-buildbuddy-api-key", f.APIKey)
			resp, err := f.Web.Client.Do(r)
			require.NoError(t, err)
			downloaded, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			if present {
				require.Equal(t, 200, resp.StatusCode, string(downloaded))
				require.Equal(t, expected, downloaded)
			} else {
				require.Equal(t, 404, resp.StatusCode, string(downloaded))
			}
		}
	}

	for _, instance := range []string{"", "ci/nested"} {
		t.Run(fmt.Sprintf("instance=%s", instance), func(t *testing.T) {

			seeds := map[string][]byte{}
			seedDirectories := map[string]*repb.Directory{}
			checkSeeds := func() {
				for _, payload := range seeds {
					digest := harness.Digest(payload)
					missing, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{digest}})
					require.NoError(t, e)
					require.Empty(t, missing.MissingBlobDigests)
					batch, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{digest}})
					require.NoError(t, e)
					require.Len(t, batch.Responses, 1)
					require.True(t, proto.Equal(digest, batch.Responses[0].Digest))
					require.Zero(t, batch.Responses[0].Status.Code)
					require.Equal(t, payload, batch.Responses[0].Data)
					b, e := c.Read(ctx, instance, digest)
					require.NoError(t, e)
					require.Equal(t, payload, b)
					uri := "bytestream://" + f.Endpoint(harness.App).InternalGRPC[len("grpc://"):] + "/" + harness.ResourceName(instance, digest, false)
					checkFile(t, uri, true, payload)
					if instance == "" {
						cmd := exec.CommandContext(ctx, "bb", "download", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey, fmt.Sprintf("%s/%d", digest.Hash, digest.SizeBytes))
						out, e := cmd.Output()
						require.NoError(t, e)
						require.Equal(t, payload, out)
					}
					if dir, ok := seedDirectories[digest.Hash]; ok {
						tr, e := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: digest})
						require.NoError(t, e)
						var dirs []*repb.Directory
						for {
							r, e := tr.Recv()
							if e == io.EOF {
								break
							}
							require.NoError(t, e)
							dirs = append(dirs, r.Directories...)
						}
						require.Len(t, dirs, 1)
						require.True(t, proto.Equal(dir, dirs[0]))
						dw, e := proto.Marshal(digest)
						require.NoError(t, e)
						treeRequest := wire(append(field(2, []byte(instance)), field(3, dw)...))
						ui, e := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree", grpc.ForceCodec(codec{}))
						require.NoError(t, e)
						require.NoError(t, ui.SendMsg(&treeRequest))
						require.NoError(t, ui.CloseSend())
						var uiDirs [][]byte
						for {
							var m wire
							e := ui.RecvMsg(&m)
							if e == io.EOF {
								break
							}
							require.NoError(t, e)
							uiDirs = append(uiDirs, byteFields(m, 2)...)
						}
						require.Equal(t, [][]byte{payload}, uiDirs)
						messages, code := webWire(t, ctx, f, "GetTree", treeRequest)
						require.Equal(t, codes.OK, code)
						var httpDirs [][]byte
						for _, m := range messages {
							httpDirs = append(httpDirs, byteFields(m, 2)...)
						}
						require.Equal(t, uiDirs, httpDirs)
						sr := append(field(2, dw), field(3, []byte(instance))...)
						sizes, e := invoke("/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", sr)
						require.NoError(t, e)
						entries := byteFields(sizes, 2)
						require.Len(t, entries, 1)
						total := digest.SizeBytes
						for _, file := range dir.Files {
							total += file.Digest.SizeBytes
						}
						require.Equal(t, uint64(total), varints(entries[0])[2])
						require.Equal(t, uint64(len(dir.Files)), varints(entries[0])[3])
						hm, code := webWire(t, ctx, f, "GetTreeDirectorySizes", sr)
						require.Equal(t, codes.OK, code)
						require.Equal(t, [][]byte{sizes}, hm)
					}

					// Every seeded Action, Command, Directory, and ordinary blob
					// is checked immediately after each upload and later mutation.
					db, e := proto.Marshal(digest)
					require.NoError(t, e)
					rn := append(field(1, db), field(2, []byte(instance))...)
					rn = append(rn, integer(4, 2)...)
					md, e := invoke("/buildbuddy.service.BuildBuddyService/GetCacheMetadata", field(2, rn))
					require.NoError(t, e)
					size := int64(len(payload))
					if digest.SizeBytes >= 100 {
						compressed, e := harness.Compress(payload)
						require.NoError(t, e)
						size = int64(len(compressed))
					}
					require.Equal(t, uint64(size), varints(md)[2])
					require.Equal(t, uint64(digest.SizeBytes), varints(md)[5])
					require.Positive(t, int64(varints(md)[3]))
					require.Positive(t, int64(varints(md)[4]))
					hm, e := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": digest.Hash, "sizeBytes": fmt.Sprint(digest.SizeBytes)}, "instanceName": instance, "cacheType": 2}})
					require.NoError(t, e)
					require.Equal(t, fmt.Sprint(size), fmt.Sprint(hm["storedSizeBytes"]))
					require.Equal(t, fmt.Sprint(digest.SizeBytes), fmt.Sprint(hm["digestSizeBytes"]))
					require.Equal(t, fmt.Sprint(varints(md)[4]), fmt.Sprint(hm["lastModifyUsec"]))
					zr, e := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, digest, true)})
					require.NoError(t, e)
					var zb []byte
					for {
						r, e := zr.Recv()
						if e == io.EOF {
							break
						}
						require.NoError(t, e)
						zb = append(zb, r.Data...)
					}
					decoded, e := harness.Decompress(zb)
					require.NoError(t, e)
					require.Equal(t, payload, decoded)
					if len(payload) >= 4 {
						rangeReq := append(field(1, []byte(uri)), integer(2, 1)...)
						rangeReq = append(rangeReq, integer(3, 4)...)
						r, e := invoke("/api.v1.ApiService/GetFileRange", rangeReq)
						require.NoError(t, e)
						require.Equal(t, payload[1:4], data(r))
						code, b := httpCall(t, "GetFileRange", map[string]any{"uri": uri, "start": 1, "end": 4})
						require.Equal(t, 200, code)
						var rangeJSON map[string]any
						require.NoError(t, json.Unmarshal(b, &rangeJSON))
						require.Equal(t, base64.StdEncoding.EncodeToString(payload[1:4]), rangeJSON["data"])
					}

				}
			}
			upload := func(payload []byte) (*repb.Digest, error) {
				digest, e := c.Upload(ctx, instance, payload)
				require.NoError(t, e)
				seeds[digest.Hash] = payload
				checkSeeds()
				return digest, e
			}
			uploadProto := func(m proto.Message) (*repb.Digest, error) {
				b, e := proto.Marshal(m)
				require.NoError(t, e)
				if dir, ok := m.(*repb.Directory); ok {
					seedDirectories[harness.Digest(b).Hash] = dir
				}
				return upload(b)
			}
			blob := []byte("immutable output for " + instance)
			d, err := upload(blob)
			require.NoError(t, err)
			other := []byte("unrelated blob " + instance)
			od, err := upload(other)
			require.NoError(t, err)
			root, err := uploadProto(&repb.Directory{Files: []*repb.FileNode{{Name: "out.txt", Digest: d}}})
			require.NoError(t, err)
			var keys []*repb.Digest
			for _, arg := range []string{"first", "second"} {
				command, err := uploadProto(&repb.Command{Arguments: []string{"sh", "-c", "printf " + arg}, OutputPaths: []string{"out.txt"}})
				require.NoError(t, err)
				action, err := uploadProto(&repb.Action{CommandDigest: command, InputRootDigest: root})
				require.NoError(t, err)
				keys = append(keys, action)
			}
			model := map[string]*repb.ActionResult{}
			casPresent := true
			resource := harness.ResourceName(instance, d, false)
			uri := "bytestream://" + f.Endpoint(harness.App).InternalGRPC[len("grpc://"):] + "/" + resource
			metadataCheck := func(d *repb.Digest, ac, present bool, payload []byte) {
				size := int64(len(payload))
				if d.SizeBytes >= 100 {
					compressed, e := harness.Compress(payload)
					require.NoError(t, e)
					size = int64(len(compressed))
				}
				db, err := proto.Marshal(d)
				require.NoError(t, err)
				ct := uint64(2)
				if ac {
					ct = 1
				}
				rn := append(field(1, db), field(2, []byte(instance))...)
				rn = append(rn, integer(4, ct)...)
				// The app's self-dialed metadata client can still be in startup
				// reconnect backoff after public gRPC readiness. Poll only
				// Unavailable, never a contradictory presence response.
				deadline := time.NewTimer(15 * time.Second)
				ticker := time.NewTicker(100 * time.Millisecond)
				defer deadline.Stop()
				defer ticker.Stop()
				for {
					var metadata wire
					metadata, err = invoke("/buildbuddy.service.BuildBuddyService/GetCacheMetadata", field(2, rn))
					if err == nil {
						require.Equal(t, size, int64(varints(metadata)[2]))
						require.Equal(t, d.SizeBytes, int64(varints(metadata)[5]))
						require.Positive(t, int64(varints(metadata)[3]))
						require.Positive(t, int64(varints(metadata)[4]))
						require.LessOrEqual(t, int64(varints(metadata)[4]), time.Now().Add(time.Second).UnixMicro())
					}
					if status.Code(err) != codes.Unavailable {
						break
					}
					select {
					case <-ticker.C:
					case <-deadline.C:
						t.Fatalf("metadata self-client not ready: %v", err)
					}
				}
				if present {
					require.NoError(t, err)
				} else {
					require.Equal(t, codes.NotFound, status.Code(err))
				}
				res, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": ct}})
				if present {
					require.NoError(t, err)
					require.Equal(t, fmt.Sprint(size), fmt.Sprint(res["storedSizeBytes"]))
					require.Equal(t, fmt.Sprint(d.SizeBytes), fmt.Sprint(res["digestSizeBytes"]))
					require.NotEmpty(t, res["lastAccessUsec"])
					require.NotEmpty(t, res["lastModifyUsec"])
				} else {
					require.Error(t, err)
					require.Contains(t, err.Error(), "NotFound")
				}
			}
			check := func() {
				checkSeeds()
				tree, e := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: root})
				require.NoError(t, e)
				var directories []*repb.Directory
				for {
					r, e := tree.Recv()
					if e == io.EOF {
						break
					}
					require.NoError(t, e)
					directories = append(directories, r.Directories...)
				}
				require.Len(t, directories, 1)
				require.True(t, proto.Equal(&repb.Directory{Files: []*repb.FileNode{{Name: "out.txt", Digest: d}}}, directories[0]))
				rootWire, e := proto.Marshal(root)
				require.NoError(t, e)
				treeMessages, treeCode := webWire(t, ctx, f, "GetTree", append(field(2, []byte(instance)), field(3, rootWire)...))
				require.Equal(t, codes.OK, treeCode)
				var treeDirs [][]byte
				for _, m := range treeMessages {
					treeDirs = append(treeDirs, byteFields(m, 2)...)
				}
				require.Len(t, treeDirs, 1)
				require.Equal(t, seeds[root.Hash], treeDirs[0])
				sizeReq := append(field(2, rootWire), field(3, []byte(instance))...)
				sizeResult, e := invoke("/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", sizeReq)
				require.NoError(t, e)
				sizeEntries := byteFields(sizeResult, 2)
				require.Len(t, sizeEntries, 1)
				require.Equal(t, uint64(root.SizeBytes+d.SizeBytes), varints(sizeEntries[0])[2])
				require.Equal(t, uint64(1), varints(sizeEntries[0])[3])
				sizeMessages, sizeCode := webWire(t, ctx, f, "GetTreeDirectorySizes", sizeReq)
				require.Equal(t, codes.OK, sizeCode)
				require.Equal(t, [][]byte{sizeResult}, sizeMessages)
				if instance == "" {
					dest := filepath.Join(t.TempDir(), "directory")
					require.NoError(t, os.MkdirAll(dest, 0755))
					cmd := exec.CommandContext(ctx, "bb", "download", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey, "--output_directory="+dest, fmt.Sprintf("%s/%d", root.Hash, root.SizeBytes))
					out, e := cmd.CombinedOutput()
					if casPresent {
						require.NoError(t, e, string(out))
						b, e := os.ReadFile(filepath.Join(dest, "out.txt"))
						require.NoError(t, e)
						require.Equal(t, blob, b)
					} else {
						require.Error(t, e)
						require.Contains(t, string(out), "NotFound")
					}
				}

				missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d, od}})
				require.NoError(t, err)
				if casPresent {
					require.Empty(t, missing.MissingBlobDigests)
				} else {
					require.Len(t, missing.MissingBlobDigests, 1)
					require.True(t, proto.Equal(d, missing.MissingBlobDigests[0]))
				}
				batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d, od}})
				require.NoError(t, err)
				require.Len(t, batch.Responses, 2)
				seen := map[string]bool{}
				for _, r := range batch.Responses {
					require.True(t, proto.Equal(r.Digest, d) || proto.Equal(r.Digest, od))
					require.False(t, seen[r.Digest.Hash], "duplicate batch digest")
					seen[r.Digest.Hash] = true
					present := casPresent || r.Digest.Hash == od.Hash
					if present {
						require.Zero(t, r.Status.Code)
						expected := blob
						if r.Digest.Hash == od.Hash {
							expected = other
						}
						require.Equal(t, expected, r.Data)
					} else {
						require.Equal(t, int32(codes.NotFound), r.Status.Code)
					}
				}
				compressed, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, true)})
				require.NoError(t, err)
				var compressedBytes []byte
				for {
					r, e := compressed.Recv()
					if e != nil {
						if casPresent {
							require.Equal(t, io.EOF, e)
						} else {
							require.Equal(t, codes.NotFound, status.Code(e))
						}
						break
					}
					compressedBytes = append(compressedBytes, r.Data...)
				}
				if casPresent {
					decoded, e := harness.Decompress(compressedBytes)
					require.NoError(t, e)
					require.Equal(t, blob, decoded)
				}
				b, err := c.Read(ctx, instance, d)
				if casPresent {
					require.NoError(t, err)
					require.Equal(t, blob, b)
				} else {
					require.Equal(t, codes.NotFound, status.Code(err))
				}
				b, err = c.Read(ctx, instance, od)
				require.NoError(t, err)
				require.Equal(t, other, b)
				rangeReq := append(field(1, []byte(uri)), integer(2, 1)...)
				rangeReq = append(rangeReq, integer(3, 4)...)
				res, err := invoke("/api.v1.ApiService/GetFileRange", rangeReq)
				if casPresent {
					require.NoError(t, err)
					require.Equal(t, blob[1:4], data(res))
				} else {
					require.Equal(t, codes.NotFound, status.Code(err))
				}
				checkFile(t, uri, casPresent, blob)
				code, out := httpCall(t, "GetFileRange", map[string]any{"uri": uri, "start": 1, "end": 4})
				if casPresent {
					require.Equal(t, 200, code)
					var r map[string]any
					require.NoError(t, json.Unmarshal(out, &r))
					require.Equal(t, base64.StdEncoding.EncodeToString(blob[1:4]), r["data"])
				} else {
					require.NotEqual(t, 200, code)
					require.Contains(t, string(out), "NotFound")
				}

				zbatch, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
				require.NoError(t, e)
				require.Len(t, zbatch.Responses, 1)
				require.True(t, proto.Equal(d, zbatch.Responses[0].Digest))
				zr := zbatch.Responses[0]
				if casPresent {
					require.Zero(t, zr.Status.Code)
					decoded := zr.Data
					if zr.Compressor == repb.Compressor_ZSTD {
						decoded, e = harness.Decompress(zr.Data)
						require.NoError(t, e)
					} else {
						require.Equal(t, repb.Compressor_IDENTITY, zr.Compressor)
					}
					require.Equal(t, blob, decoded)
				} else {
					require.Equal(t, int32(codes.NotFound), zr.Status.Code)
				}
				rangeStream, e := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: resource, ReadOffset: 1, ReadLimit: 3})
				require.NoError(t, e)
				var rangeBytes []byte
				for {
					r, e := rangeStream.Recv()
					if e != nil {
						if casPresent {
							require.Equal(t, io.EOF, e)
						} else {
							require.Equal(t, codes.NotFound, status.Code(e))
						}
						break
					}
					rangeBytes = append(rangeBytes, r.Data...)
				}
				if casPresent {
					require.Equal(t, blob[1:4], rangeBytes)
				}
				metadataCheck(d, false, casPresent, blob)
				metadataCheck(od, false, true, other)
				if instance == "" {
					cmd := exec.CommandContext(ctx, "bb", "download", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey, "/"+resource)
					var stderr bytes.Buffer
					cmd.Stderr = &stderr
					out, err := cmd.Output()
					if casPresent {
						require.NoError(t, err)
						require.Equal(t, blob, out)
					} else {
						require.Error(t, err)
						require.Contains(t, stderr.String(), "NotFound")
					}
				}
				for _, key := range keys {
					got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: key})
					expected, present := model[key.Hash]
					if present && casPresent {
						require.NoError(t, err)
						require.True(t, proto.Equal(expected, got), "AC result differs: %v", got)
						encodedResult, e := proto.Marshal(expected)
						require.NoError(t, e)
						resultDigest := harness.Digest(encodedResult)
						digestWire, e := proto.Marshal(resultDigest)
						require.NoError(t, e)
						conditionalReq, e := proto.Marshal(&repb.GetActionResultRequest{InstanceName: instance, ActionDigest: key})
						require.NoError(t, e)
						conditional, e := invoke("/build.bazel.remote.execution.v2.ActionCache/GetActionResult", append(conditionalReq, field(1001, digestWire)...))
						require.NoError(t, e)
						require.Equal(t, field(1000, digestWire), []byte(conditional))
						mismatchWire, e := proto.Marshal(harness.Digest([]byte("older result")))
						require.NoError(t, e)
						conditional, e = invoke("/build.bazel.remote.execution.v2.ActionCache/GetActionResult", append(conditionalReq, field(1001, mismatchWire)...))
						require.NoError(t, e)
						var full repb.ActionResult
						require.NoError(t, proto.Unmarshal(conditional, &full))
						require.True(t, proto.Equal(expected, &full))

						inline, e := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: key, InlineOutputFiles: []string{"out.txt"}, InlineStdout: true, InlineStderr: true})
						require.NoError(t, e)
						want := proto.Clone(expected).(*repb.ActionResult)
						want.OutputFiles[0].Contents = blob
						require.True(t, proto.Equal(want, inline), "inline result differs: %v", inline)
					} else {
						require.Equal(t, codes.NotFound, status.Code(err))
					}
					var metadataBytes []byte
					if present {
						metadataBytes, err = proto.Marshal(expected)
						require.NoError(t, err)
					}
					metadataCheck(key, true, present, metadataBytes)
					suffix := fmt.Sprintf("%s/%d", key.Hash, key.SizeBytes)
					rn := harness.ResourceName(instance, key, false)
					acURI := "actioncache://" + f.Endpoint(harness.App).InternalGRPC[len("grpc://"):] + "/" + rn[:len(rn)-len(suffix)] + "ac/" + suffix
					var encoded []byte
					if present {
						encoded, err = proto.Marshal(expected)
						require.NoError(t, err)
					}
					checkFile(t, acURI, present && casPresent, encoded)
				}
			}
			update := func(key *repb.Digest, exit int32) {
				result := &repb.ActionResult{StdoutRaw: []byte("fixture stdout"), StderrRaw: []byte("fixture stderr"), ExitCode: exit, ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: "fixture-worker"}, OutputFiles: []*repb.OutputFile{{Path: "out.txt", Digest: d}}}
				got, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: key, ActionResult: result})
				require.NoError(t, err)
				require.True(t, proto.Equal(result, got))
				model[key.Hash] = result
				check()
			}
			deleteFile := func(name string, rest bool) {
				if rest {
					code, b := httpCall(t, "DeleteFile", map[string]any{"uri": name})
					require.Equal(t, 200, code, string(b))
				} else {
					_, err := invoke("/api.v1.ApiService/DeleteFile", field(1, []byte(name)))
					require.NoError(t, err)
				}
			}
			check()
			update(keys[0], 0)
			update(keys[1], 0)
			update(keys[0], 7)
			update(keys[0], 7)
			acName := harness.ResourceName(instance, keys[0], false) // Insert AC segment before hash.
			prefix := harness.ResourceName(instance, keys[0], false)
			acName = prefix[:len(prefix)-len(fmt.Sprintf("%s/%d", keys[0].Hash, keys[0].SizeBytes))] + "ac/" + fmt.Sprintf("%s/%d", keys[0].Hash, keys[0].SizeBytes)
			for _, rest := range []bool{false, true} {
				deleteFile(acName, rest)
				delete(model, keys[0].Hash)
				check()
				deleteFile(acName, rest)
				check()
				update(keys[0], 0)
			}
			update(keys[0], 0)
			// Preserve AC references while deleting an output: validated REAPI AC
			// must miss, while raw AC storage remains observable.
			deleteFile(resource, false)
			casPresent = false
			delete(seeds, d.Hash)
			check()
			_, err = c.Write(ctx, instance, blob, false)
			require.NoError(t, err)
			casPresent = true
			seeds[d.Hash] = blob
			check()
			// Independently delete/recreate CAS after removing AC references.
			for _, key := range keys {
				name := harness.ResourceName(instance, key, false)
				suffix := fmt.Sprintf("%s/%d", key.Hash, key.SizeBytes)
				deleteFile(name[:len(name)-len(suffix)]+"ac/"+suffix, true)
				delete(model, key.Hash)
				check()
			}
			for _, rest := range []bool{false, true} {
				deleteFile(resource, rest)
				casPresent = false
				delete(seeds, d.Hash)
				check()
				deleteFile(resource, rest)
				check()
				_, err := c.Write(ctx, instance, blob, false)
				require.NoError(t, err)
				casPresent = true
				seeds[d.Hash] = blob
				check()
			}
		})
	}
}

func publishInvocation(t *testing.T, ctx context.Context, c *harness.Clients, id string) {
	t.Helper()
	started := field(1, []byte(id))
	started = append(started, integer(2, uint64(time.Now().UnixMilli()))...)
	started = append(started, field(3, []byte("8.0.0"))...)
	started = append(started, field(4, []byte("--color=no"))...)
	started = append(started, field(5, []byte("build"))...)
	first := append(field(1, field(3, nil)), field(2, field(9, nil))...)
	first = append(first, field(5, started)...)
	// Supply an actual finish time, as a real Bazel producer does.
	finishPayload := append(field(3, append(field(1, []byte("SUCCESS")), integer(2, 0)...)), integer(2, uint64(time.Now().UnixMilli()))...)
	finished := append(field(1, field(9, nil)), field(14, finishPayload)...)
	finished = append(finished, integer(20, 1)...)
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

func varints(b []byte) map[protowire.Number]uint64 {
	r := map[protowire.Number]uint64{}
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("invalid wire tag")
		}
		b = b[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				panic("invalid varint")
			}
			r[n] = v
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			if k < 0 {
				panic("invalid wire field")
			}
			b = b[k:]
		}
	}
	return r
}
