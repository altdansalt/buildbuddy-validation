package convergence_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type model struct {
	cas bool
	ac  *repb.ActionResult
}

func sameResult(want, got *repb.ActionResult) bool {
	if want == nil || got == nil {
		return want == got
	}
	w := proto.Clone(want).(*repb.ActionResult)
	g := proto.Clone(got).(*repb.ActionResult)
	w.ExecutionMetadata = nil
	g.ExecutionMetadata = nil
	return proto.Equal(w, g)
}

func TestDeletionRecreation(t *testing.T) {
	for _, instance := range []string{"", "delete-conformance"} {
		t.Run("instance="+instance, func(t *testing.T) { runSequence(t, instance) })
	}
}
func runSequence(t *testing.T, instance string) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	key2 := f.Web.CreateAPIKey(t, f.GroupID, "second-state-client", []string{"CACHE_WRITE"})
	app2, proxy2 := harness.Dial(t, f.Endpoint(harness.App).GRPC, key2), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key2)
	fileData := []byte("immutable CAS deletion and recreation conformance\n")
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entryWriter, e := zw.CreateHeader(&zip.FileHeader{Name: "state.txt", Method: zip.Store})
	require.NoError(t, e)
	_, e = entryWriter.Write(fileData)
	require.NoError(t, e)
	require.NoError(t, zw.Close())
	data := archive.Bytes()
	zr, e := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, e)
	offset, e := zr.File[0].DataOffset()
	require.NoError(t, e)
	zipEntry := number(number(number(number(number(field(nil, 1, []byte("state.txt")), 2, uint64(len(fileData))), 3, uint64(len(fileData))), 5, uint64(offset)), 6, 1), 7, uint64(crc32.ChecksumIEEE(fileData)))
	d := harness.Digest(data)
	cmdDigest, e := app.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/echo", "conformance"}})
	require.NoError(t, e)
	checkSetupBlob(t, ctx, f, instance, cmdDigest, &repb.Command{Arguments: []string{"/bin/echo", "conformance"}})
	rootDigest, e := app.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, e)
	checkSetupBlob(t, ctx, f, instance, rootDigest, &repb.Directory{})
	ad, e := app.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmdDigest, InputRootDigest: rootDigest})
	require.NoError(t, e)

	checkSetupBlob(t, ctx, f, instance, ad, &repb.Action{CommandDigest: cmdDigest, InputRootDigest: rootDigest})
	treeDigest, e := app.UploadProto(ctx, instance, &repb.Tree{Root: &repb.Directory{}})
	require.NoError(t, e)
	checkSetupBlob(t, ctx, f, instance, treeDigest, &repb.Tree{Root: &repb.Directory{}})
	alternateTree, e := app.UploadProto(ctx, instance, &repb.Tree{Root: &repb.Directory{Files: []*repb.FileNode{{Name: "output", Digest: cmdDigest}}}})
	require.NoError(t, e)
	checkSetupBlob(t, ctx, f, instance, alternateTree, &repb.Tree{Root: &repb.Directory{Files: []*repb.FileNode{{Name: "output", Digest: cmdDigest}}}})
	commandBytes, e := proto.Marshal(&repb.Command{Arguments: []string{"/bin/echo", "conformance"}})
	require.NoError(t, e)
	actionBytes, e := proto.Marshal(&repb.Action{CommandDigest: cmdDigest, InputRootDigest: rootDigest})
	require.NoError(t, e)
	referenceOutput := map[string][]byte{cmdDigest.Hash: commandBytes, ad.Hash: actionBytes}
	uri := harness.ResourceName(instance, d, false)
	acuri := fmt.Sprintf("blobs/ac/%s/%d", ad.Hash, ad.SizeBytes)
	if instance != "" {
		acuri = instance + "/" + acuri
	}
	invocationID := fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	started := field(field(field(nil, 1, []byte(invocationID)), 4, []byte("--build_metadata=ROLE=CONFORMANCE")), 5, []byte("test"))
	events := [][]byte{
		field(field(nil, 1, field(nil, 3, nil)), 5, started),
		number(field(field(nil, 1, field(nil, 9, nil)), 14, field(nil, 3, field(nil, 1, []byte("SUCCESS")))), 20, 1),
	}
	besStream, e := app.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, e)
	for i, event := range events {
		require.NoError(t, besStream.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: &bes.StreamId{BuildId: invocationID, InvocationId: invocationID, Component: bes.StreamId_TOOL}, SequenceNumber: int64(i + 1), Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: event}}}}}))
	}
	require.NoError(t, besStream.CloseSend())
	for range events {
		_, e := besStream.Recv()
		require.NoError(t, e)
	}
	_, e = besStream.Recv()
	require.Equal(t, io.EOF, e)
	state := model{}
	httpCall := func(method string, payload any) (int, []byte) {
		t.Helper()
		b, err := json.Marshal(payload)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(b))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		out, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, out
	}
	deleteFile := func(uri string, rest bool) {
		t.Helper()
		if rest {
			code, b := httpCall("DeleteFile", map[string]any{"uri": uri})
			require.Equal(t, 200, code, string(b))
			return
		}
		require.NoError(t, app2.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", wrapperspb.String(uri), &emptypb.Empty{}))
	}
	// Check the model at every exposed authoritative surface. CAS retention in a
	// warmed proxy is allowed: deleting backing immutable bytes does not invalidate it.
	check := func(warmed bool) {
		t.Helper()
		for _, c := range []*harness.Clients{app, proxy, app2, proxy2} {
			present := state.cas || ((c == proxy || c == proxy2) && warmed)
			fm, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
			require.NoError(t, err)
			// With default TTL=0, FindMissing is authoritative even when local bytes remain.
			if state.cas {
				require.Empty(t, fm.MissingBlobDigests)
			} else {
				require.Len(t, fm.MissingBlobDigests, 1)
				require.True(t, proto.Equal(d, fm.MissingBlobDigests[0]))
			}
			_, qerr := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: harness.UploadResourceName(instance, d, false)})
			require.Equal(t, codes.Unimplemented, status.Code(qerr))
			br, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
			require.NoError(t, err)
			require.Len(t, br.Responses, 1)
			require.True(t, proto.Equal(d, br.Responses[0].Digest))
			b, err := c.Read(ctx, instance, d)
			if present {
				require.NoError(t, err)
				require.Equal(t, data, b)
				require.Equal(t, int32(0), br.Responses[0].GetStatus().GetCode())
				require.Equal(t, data, br.Responses[0].Data)
			} else {
				require.Equal(t, codes.NotFound, status.Code(err))
				require.Equal(t, int32(codes.NotFound), br.Responses[0].GetStatus().GetCode())
			}

			for _, compressed := range []bool{false, true} {
				request := &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, compressed)}
				if !compressed {
					request.ReadOffset = 3
					request.ReadLimit = 16
				}
				stream, err := c.ByteStream.Read(ctx, request)
				require.NoError(t, err)
				var payload []byte
				for {
					r, e := stream.Recv()
					if e != nil {
						err = e
						break
					}
					payload = append(payload, r.Data...)
				}
				if present {
					require.Equal(t, io.EOF, err)
					expected := data[3:19]
					if compressed {
						payload, err = harness.Decompress(payload)
						require.NoError(t, err)
						expected = data
					}
					require.Equal(t, expected, payload)
				} else {
					require.Equal(t, codes.NotFound, status.Code(err))
				}
			}
			// Poll AC against a reference revision; no sleeps and a strict bounded deadline.
			for _, ttl := range []string{"200ms", "0s"} {
				readCtx := metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-platform.cache-proxy-action-cache-ttl", ttl)
				started := time.Now()
				pollCtx, stop := context.WithTimeout(readCtx, 10*time.Second)
				ticker := time.NewTicker(100 * time.Millisecond)
				for {
					ar, e := c.AC.GetActionResult(pollCtx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: ad})
					if (state.ac == nil && status.Code(e) == codes.NotFound) || (state.ac != nil && e == nil && sameResult(state.ac, ar)) {
						break
					}
					select {
					case <-pollCtx.Done():
						t.Fatalf("AC did not converge: result=%v err=%v want=%v", ar, e, state.ac)
					case <-ticker.C:
					}
				}
				t.Logf("AC convergence app=%v ttl=%s observed=%s", c == app || c == app2, ttl, time.Since(started))
				for stable := 0; stable < 3; stable++ {
					<-ticker.C
					ar, err := c.AC.GetActionResult(pollCtx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: ad})
					if state.ac == nil {
						require.Equal(t, codes.NotFound, status.Code(err))
					} else {
						require.NoError(t, err)
						require.True(t, sameResult(state.ac, ar), "unstable converged AC: %v", ar)
					}
				}
				ticker.Stop()
				stop()
			}

			if state.ac != nil {
				ar, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: ad, InlineStdout: true, InlineStderr: true, InlineOutputFiles: []string{"output"}})
				require.NoError(t, err)
				expected := proto.Clone(state.ac).(*repb.ActionResult)
				expected.OutputFiles[0].Contents = referenceOutput[expected.OutputFiles[0].Digest.Hash]
				require.True(t, sameResult(expected, ar), "inline ActionResult mismatch: %v", ar)
			}
		}
		for _, origin := range []harness.Component{harness.App, harness.CacheProxy} {
			visibleCAS := state.cas || (origin == harness.CacheProxy && warmed)
			fullURI := "bytestream://" + f.APIKey + "@" + strings.TrimPrefix(f.Endpoint(origin).InternalGRPC, "grpc://") + "/" + uri
			manifest := &emptypb.Empty{}
			manifestErr := app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetZipManifest", wire(field(nil, 2, []byte(fullURI))), manifest)
			webManifest, webManifestErr := f.Web.RPC("GetZipManifest", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "uri": fullURI})
			if visibleCAS {
				require.NoError(t, manifestErr)
				require.NoError(t, webManifestErr)
				manifests := fields(t, manifest.ProtoReflect().GetUnknown(), 2)
				require.Len(t, manifests, 1)
				entries := fields(t, manifests[0], 1)
				require.Len(t, entries, 1)
				require.Equal(t, "state.txt", string(fields(t, entries[0], 1)[0]))
				for _, n := range []protowire.Number{2, 3, 4, 5, 6, 7} {
					expectedField := integer(t, zipEntry, n)
					if n == 5 {
						expectedField = 0
					} // Known omitted content_offset; zip_offset_repro asserts the documented value.
					require.Equal(t, expectedField, integer(t, entries[0], n))
				}
				webEntries := webManifest["manifest"].(map[string]any)["entry"].([]any)
				require.Len(t, webEntries, 1)
				webEntry := webEntries[0].(map[string]any)
				require.Equal(t, "state.txt", webEntry["name"])
				require.Equal(t, fmt.Sprint(len(fileData)), fmt.Sprint(webEntry["uncompressedSize"]))
			} else {
				require.Equal(t, codes.Unavailable, status.Code(manifestErr))
				require.ErrorContains(t, webManifestErr, "Unavailable")
			}
			for _, path := range []string{"/file/download", "/file/view"} {
				req, err := http.NewRequestWithContext(ctx, "GET", f.Endpoint(harness.App).HTTP+path+"?bytestream_url="+url.QueryEscape(fullURI)+"&invocation_id="+invocationID+"&z="+url.QueryEscape(base64.StdEncoding.EncodeToString(zipEntry)), nil)
				require.NoError(t, err)
				req.Header.Set("x-buildbuddy-api-key", f.APIKey)
				res, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				extracted, err := io.ReadAll(res.Body)
				res.Body.Close()
				require.NoError(t, err)
				if visibleCAS {
					require.Equal(t, 200, res.StatusCode, string(extracted))
					require.Equal(t, fileData, extracted)
				} else {
					require.Equal(t, 404, res.StatusCode, string(extracted))
				}
			}

			req := wrapperspb.String(fullURI)
			req.ProtoReflect().SetUnknown(number(number(nil, 2, 3), 3, 19))
			out := &wrapperspb.BytesValue{}
			// GetFileRange reads the app's own cache and ignores the URI host.
			err := app.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", req, out)
			if state.cas {
				require.NoError(t, err)
				require.Equal(t, data[3:19], out.Value)
			} else {
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			rangeCode, rangeBody := httpCall("GetFileRange", map[string]any{"uri": fullURI, "start": "3", "end": "19"})
			if state.cas {
				require.Equal(t, 200, rangeCode, string(rangeBody))
				var result map[string]any
				require.NoError(t, json.Unmarshal(rangeBody, &result))
				require.Equal(t, base64.StdEncoding.EncodeToString(data[3:19]), result["data"])
			} else {
				require.Equal(t, 500, rangeCode, string(rangeBody))
				require.Contains(t, string(rangeBody), "code = NotFound")
			}
			code, b := httpCall("GetFile", map[string]any{"uri": fullURI})
			if visibleCAS {
				require.Equal(t, 200, code, string(b))
				require.Equal(t, data, b)
			} else {
				require.Equal(t, 404, code, string(b))
			}

			stream, e := app.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
			require.NoError(t, e)
			require.NoError(t, stream.SendMsg(wrapperspb.String(fullURI)))
			require.NoError(t, stream.CloseSend())
			var got []byte
			for {
				r := &wrapperspb.BytesValue{}
				e = stream.RecvMsg(r)
				if e != nil {
					break
				}
				got = append(got, r.Value...)
			}
			if visibleCAS {
				require.Equal(t, io.EOF, e)
				require.Equal(t, data, got)
			} else {
				require.Equal(t, codes.Unavailable, status.Code(e))
			}
			for _, path := range []string{"/file/download", "/file/view"} {
				req, e := http.NewRequestWithContext(ctx, "GET", f.Endpoint(harness.App).HTTP+path+"?bytestream_url="+url.QueryEscape(fullURI)+"&invocation_id="+invocationID, nil)
				require.NoError(t, e)
				req.Header.Set("x-buildbuddy-api-key", f.APIKey)
				res, e := http.DefaultClient.Do(req)
				require.NoError(t, e)
				b, e := io.ReadAll(res.Body)
				res.Body.Close()
				require.NoError(t, e)
				if visibleCAS {
					require.Equal(t, 200, res.StatusCode, string(b))
					require.Equal(t, data, b)
				} else {
					require.Equal(t, 404, res.StatusCode, string(b))
				}
			}
			{
				bb, e := exec.LookPath("bb")
				require.NoError(t, e, "CLI coverage is required")
				{
					for _, c := range []harness.Component{harness.App, harness.CacheProxy} {
						output := t.TempDir() + "/download"
						cmd := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(c).GRPC, "--api_key="+f.APIKey, "--output_file="+output, "/"+uri)
						log, e := cmd.CombinedOutput()
						if state.cas || (c == harness.CacheProxy && warmed) {
							require.NoError(t, e, string(log))
							b, e := os.ReadFile(output)
							require.NoError(t, e)
							require.Equal(t, data, b)
						} else {
							require.Error(t, e, string(log))
							require.Contains(t, string(log), "NotFound")
						}
					}
				}
			}
			acURL := "actioncache://" + f.APIKey + "@" + strings.TrimPrefix(f.Endpoint(origin).InternalGRPC, "grpc://") + "/" + acuri
			acStream, e := app.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
			require.NoError(t, e)
			require.NoError(t, acStream.SendMsg(wrapperspb.String(acURL)))
			require.NoError(t, acStream.CloseSend())
			var acBytes []byte
			for {
				r := &wrapperspb.BytesValue{}
				e = acStream.RecvMsg(r)
				if e != nil {
					break
				}
				acBytes = append(acBytes, r.Value...)
			}
			if state.ac != nil {
				require.Equal(t, io.EOF, e)
				ar := &repb.ActionResult{}
				require.NoError(t, proto.Unmarshal(acBytes, ar))
				require.True(t, sameResult(state.ac, ar), "AC file response: %v", ar)
			} else {
				require.Equal(t, codes.Unavailable, status.Code(e))
			}
			code, b = httpCall("GetFile", map[string]any{"uri": acURL})
			if state.ac != nil {
				require.Equal(t, 200, code, string(b))
				ar := &repb.ActionResult{}
				require.NoError(t, proto.Unmarshal(b, ar))
				require.True(t, sameResult(state.ac, ar), "AC file response: %v", ar)
			} else {
				require.Equal(t, 404, code, string(b))
			}
			for _, path := range []string{"/file/download", "/file/view"} {
				req, e := http.NewRequestWithContext(ctx, "GET", f.Endpoint(harness.App).HTTP+path+"?bytestream_url="+url.QueryEscape(acURL)+"&invocation_id="+invocationID, nil)
				require.NoError(t, e)
				req.Header.Set("x-buildbuddy-api-key", f.APIKey)
				res, e := http.DefaultClient.Do(req)
				require.NoError(t, e)
				b, e := io.ReadAll(res.Body)
				res.Body.Close()
				require.NoError(t, e)
				if state.ac != nil {
					require.Equal(t, 200, res.StatusCode, string(b))
					ar := &repb.ActionResult{}
					require.NoError(t, proto.Unmarshal(b, ar))
					require.True(t, sameResult(state.ac, ar), "AC file response: %v", ar)
				} else {
					require.Equal(t, 404, res.StatusCode, string(b))
				}
			}

		}
		for _, ac := range []bool{false, true} {
			digest := d
			exists := state.cas
			if ac {
				digest = ad
				exists = state.ac != nil
			}
			meta := &emptypb.Empty{}
			err := app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", wire(field(nil, 2, resource(digest, instance, ac))), meta)
			if exists {
				require.NoError(t, err)
				expectedSize := digest.SizeBytes
				if ac {
					expectedSize = digest.SizeBytes
					checkMetadata(t, meta.ProtoReflect().GetUnknown(), expectedSize)
				} else {
					checkMetadata(t, meta.ProtoReflect().GetUnknown(), expectedSize)
				}
			} else {
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			typ := "CAS"
			if ac {
				typ = "AC"
			}
			for _, c := range []*harness.Clients{app, proxy, app2, proxy2} {
				response := &emptypb.Empty{}
				e := c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", wire(field(nil, 2, resource(digest, instance, ac))), response)
				visible := exists
				// Proxy metadata describes local storage, where AC keys are salted request hashes.
				if c == proxy || c == proxy2 {
					visible = !ac && (state.cas || warmed)
				}
				if visible {
					require.NoError(t, e)
					expectedSize := digest.SizeBytes
					if ac {
						expectedSize = digest.SizeBytes
					}
					checkMetadata(t, response.ProtoReflect().GetUnknown(), expectedSize)
				} else {
					require.Equal(t, codes.NotFound, status.Code(e))
				}
			}
			webMeta, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": digest.Hash, "sizeBytes": fmt.Sprint(digest.SizeBytes)}, "instanceName": instance, "cacheType": typ}})
			if exists {
				require.NoError(t, err)
				expectedSize := digest.SizeBytes
				if ac {
					expectedSize = digest.SizeBytes
				}
				checkWebMetadata(t, webMeta, expectedSize)

			} else {
				require.ErrorContains(t, err, "NotFound")
			}
		}

		// Recheck logical replicas after all HTTP/CLI observations to catch a
		// converged result that changes again while the model remains fixed.
		for _, c := range []*harness.Clients{app, proxy, app2, proxy2} {
			fm, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
			require.NoError(t, err)
			require.Equal(t, !state.cas, len(fm.MissingBlobDigests) == 1)
			body, err := c.Read(ctx, instance, d)
			if state.cas || ((c == proxy || c == proxy2) && warmed) {
				require.NoError(t, err)
				require.Equal(t, data, body)
			} else {
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			ar, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: ad})
			if state.ac == nil {
				require.Equal(t, codes.NotFound, status.Code(err))
			} else {
				require.NoError(t, err)
				require.True(t, sameResult(state.ac, ar))
			}
		}
	}

	update := func(c *harness.Clients, revision string) {
		next := &repb.ActionResult{StdoutRaw: []byte(revision), StderrRaw: []byte("stderr " + revision), ExitCode: int32(len(revision)), OutputFiles: []*repb.OutputFile{{Path: "output", Digest: cmdDigest, IsExecutable: true}}, OutputDirectories: []*repb.OutputDirectory{{Path: "dir", TreeDigest: treeDigest}}, OutputFileSymlinks: []*repb.OutputSymlink{{Path: "link", Target: "output"}}}
		if revision == "revision two" {
			next.OutputFiles[0].Digest = ad
			next.OutputFiles[0].IsExecutable = false
			next.OutputDirectories[0].TreeDigest = alternateTree
			next.OutputFileSymlinks[0].Target = "dir/output"
		}
		if revision == "revision three" {
			next.StdoutRaw = nil
			next.StderrRaw = nil
			next.StdoutDigest = cmdDigest
			next.StderrDigest = cmdDigest
		}
		_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: ad, ActionResult: next})
		require.NoError(t, err)
		state.ac = next
	}
	check(false)
	_, err := app.Upload(ctx, instance, data)
	require.NoError(t, err)
	state.cas = true
	check(false)
	update(app, "revision one")
	check(false) // warms proxy CAS and AC
	// Repeat valid writes, then change mutable AC via the proxy.
	_, err = app.Write(ctx, instance, data, false)
	require.NoError(t, err)
	check(true)
	update(app, "revision one")
	check(true)
	update(proxy2, "revision two")
	check(true)
	update(app2, "revision three")
	check(true)
	for _, rest := range []bool{false, true} {
		deleteURI := uri
		if rest {
			deleteURI = strings.Replace(uri, "blobs/", "compressed-blobs/zstd/", 1)
		}
		deleteFile(deleteURI, rest)
		state.cas = false
		check(true)
		deleteFile(acuri, rest)
		state.ac = nil
		check(true)
		deleteFile(deleteURI, rest)
		check(true)
		deleteFile(acuri, rest)
		check(true)
		if rest {
			_, err = proxy.Write(ctx, instance, data, true)
		} else {
			_, err = proxy.Upload(ctx, instance, data)
		}
		require.NoError(t, err)
		state.cas = true
		check(true)
		update(app, "recreated revision")
		check(true)
	}
}
