package convergence_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// The scaffold has no BuildBuddy proto bindings. These helpers encode the exact
// released proto field numbers; wrappers are wire-compatible with uri/data = 1.
func field(b []byte, n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, n, protowire.BytesType), v)
}
func number(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}
func resource(d *repb.Digest, instance string, ac bool) []byte {
	b, _ := proto.Marshal(d)
	r := field(nil, 1, b)
	r = field(r, 2, []byte(instance))
	typ := uint64(2)
	if ac {
		typ = 1
	}
	return number(r, 4, typ)
}
func wire(b []byte) *emptypb.Empty { m := &emptypb.Empty{}; m.ProtoReflect().SetUnknown(b); return m }

type model struct {
	cas bool
	ac  *repb.ActionResult
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
	data := []byte("immutable CAS deletion and recreation conformance\n")
	d := harness.Digest(data)
	cmdDigest, e := app.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/echo", "conformance"}})
	require.NoError(t, e)
	rootDigest, e := app.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, e)
	ad, e := app.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmdDigest, InputRootDigest: rootDigest})
	require.NoError(t, e)

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
		require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", wrapperspb.String(uri), &emptypb.Empty{}))
	}
	// Check the model at every exposed authoritative surface. CAS retention in a
	// warmed proxy is allowed: deleting backing immutable bytes does not invalidate it.
	check := func(warmed bool) {
		t.Helper()
		for _, c := range []*harness.Clients{app, proxy} {
			present := state.cas || (c == proxy && warmed)
			fm, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
			require.NoError(t, err)
			// With default TTL=0, FindMissing is authoritative even when local bytes remain.
			require.Equal(t, !state.cas, len(fm.MissingBlobDigests) == 1)
			br, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
			require.NoError(t, err)
			require.Len(t, br.Responses, 1)
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
			// Poll AC against a reference revision; no sleeps and a strict bounded deadline.
			for _, ttl := range []string{"200ms", "0s"} {
				readCtx := metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-platform.cache-proxy-action-cache-ttl", ttl)
				pollCtx, stop := context.WithTimeout(readCtx, 10*time.Second)
				ticker := time.NewTicker(50 * time.Millisecond)
				for {
					ar, e := c.AC.GetActionResult(pollCtx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: ad})
					if (state.ac == nil && status.Code(e) == codes.NotFound) || (state.ac != nil && e == nil && bytes.Equal(state.ac.StdoutRaw, ar.StdoutRaw) && state.ac.ExitCode == ar.ExitCode) {
						break
					}
					select {
					case <-pollCtx.Done():
						t.Fatalf("AC did not converge: result=%v err=%v want=%v", ar, e, state.ac)
					case <-ticker.C:
					}
				}
				ticker.Stop()
				stop()
			}
		}
		fullURI := "bytestream://" + f.APIKey + "@" + strings.TrimPrefix(f.Endpoint(harness.App).InternalGRPC, "grpc://") + "/" + uri
		req := wrapperspb.String(fullURI)
		req.ProtoReflect().SetUnknown(number(number(nil, 2, 0), 3, uint64(len(data))))
		out := &wrapperspb.BytesValue{}
		err := app.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", req, out)
		if state.cas {
			require.NoError(t, err)
			require.Equal(t, data, out.Value)
		} else {
			require.Equal(t, codes.NotFound, status.Code(err))
		}
		rangeCode, rangeBody := httpCall("GetFileRange", map[string]any{"uri": fullURI, "start": "0", "end": fmt.Sprint(len(data))})
		if state.cas {
			require.Equal(t, 200, rangeCode, string(rangeBody))
			var result map[string]any
			require.NoError(t, json.Unmarshal(rangeBody, &result))
			require.Equal(t, base64.StdEncoding.EncodeToString(data), result["data"])
		} else {
			require.NotEqual(t, 200, rangeCode, string(rangeBody))
		}
		code, b := httpCall("GetFile", map[string]any{"uri": fullURI})
		if state.cas {
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
		if state.cas {
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
			if state.cas {
				require.Equal(t, 200, res.StatusCode, string(b))
				require.Equal(t, data, b)
			} else {
				require.Equal(t, 404, res.StatusCode, string(b))
			}
		}
		if instance == "" {
			if bb, e := exec.LookPath("bb"); e == nil {
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
					}
				}
			} else {
				t.Log("bb CLI unavailable on runner; ByteStream equivalent covered")
			}
		}
		acURL := "actioncache://" + f.APIKey + "@" + strings.TrimPrefix(f.Endpoint(harness.App).InternalGRPC, "grpc://") + "/" + acuri
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
			require.Equal(t, state.ac.StdoutRaw, ar.StdoutRaw)
		} else {
			require.Equal(t, codes.Unavailable, status.Code(e))
		}
		code, b = httpCall("GetFile", map[string]any{"uri": acURL})
		if state.ac != nil {
			require.Equal(t, 200, code, string(b))
			ar := &repb.ActionResult{}
			require.NoError(t, proto.Unmarshal(b, ar))
			require.Equal(t, state.ac.StdoutRaw, ar.StdoutRaw)
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
				require.Equal(t, state.ac.StdoutRaw, ar.StdoutRaw)
			} else {
				require.Equal(t, 404, res.StatusCode, string(b))
			}
		}
		for _, ac := range []bool{false, true} {
			digest := d
			exists := state.cas
			if ac {
				digest = ad
				exists = state.ac != nil
			}
			err := app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", wire(field(nil, 2, resource(digest, instance, ac))), &emptypb.Empty{})
			if exists {
				require.NoError(t, err)
			} else {
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			typ := "CAS"
			if ac {
				typ = "AC"
			}
			for _, c := range []*harness.Clients{app, proxy} {
				response := &emptypb.Empty{}
				e := c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", wire(field(nil, 2, resource(digest, instance, ac))), response)
				visible := exists
				// Proxy metadata describes local storage, where AC keys are salted request hashes.
				if c == proxy {
					visible = !ac && (state.cas || warmed)
				}
				if visible {
					require.NoError(t, e)
				} else {
					require.Equal(t, codes.NotFound, status.Code(e))
				}
			}
			_, err = f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": digest.Hash, "sizeBytes": fmt.Sprint(digest.SizeBytes)}, "instanceName": instance, "cacheType": typ}})
			if exists {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "NotFound")
			}
		}
	}
	update := func(c *harness.Clients, revision string) {
		next := &repb.ActionResult{StdoutRaw: []byte(revision)}
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
	update(proxy, "revision two")
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
