package distributedcache_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/jhump/protoreflect/desc/protoparse"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Wire-compatible projections of the released protos. Omitted fields are not
// used by this fixture (no GCS references, compression, or hinted handoffs).
const schema = `syntax="proto3"; package projection;
message Digest { string hash=1; int64 size_bytes=2; }
message Resource { Digest digest=1; string instance_name=2; int32 cache_type=4; int32 digest_function=5; }
message Request { Resource resource=3; }
message ReadRequest { Resource resource=6; int64 offset=3; int64 limit=5; }
message WriteRequest { bool finish_write=3; bytes data=4; Resource resource=7; bool check_already_exists=8; }
message WriteResponse { int64 committed_size=1; }
message ReadResponse { bytes data=1; }
message BSResponse { bytes data=10; }
message BSRead { string resource_name=1; int64 read_offset=2; int64 read_limit=3; }
message Metadata { int64 stored_size_bytes=1; int64 last_access_usec=2; int64 last_modify_usec=3; int64 digest_size_bytes=4; }
message WithRequest { Resource resource=1; }
message WithResponse { bytes data=1; Metadata metadata=2; }
message Key { string key=1; int64 size_bytes=2; }
message MissingRequest { repeated Resource resources=3; }
message MissingResponse { repeated Key missing=1; }
message MultiRequest { repeated Resource resources=4; }
message KV { Key key=1; bytes value=2; }
message MultiResponse { repeated KV key_value=1; }
message Heartbeat { string source=1; }
message Empty {}
message UIRequest { Resource resource_name=2; }
message UIResponse { int64 stored_size_bytes=2; int64 last_access_usec=3; int64 last_modify_usec=4; int64 digest_size_bytes=5; }
message AuthRequest { int32 jwt_signing_method=2; }
message AuthResponse { string jwt=1; }
message FileRequest { string uri=1; int64 start=2; int64 end=3; }
message Symlink { string name=1; string target=2; }
message Directory { repeated Symlink symlinks=3; }
message TreeRequest { string instance_name=1; Digest root_digest=2; }
message TreeResponse { repeated Directory directories=1; }
message UITreeRequest { string instance_name=2; Digest root_digest=3; }
message UITreeResponse { repeated Directory directories=2; }
message SizeRequest { Digest root_digest=2; string instance_name=3; }
message Size { string digest=1; int64 total_size=2; int64 child_count=3; }
message Sizes { repeated Size sizes=2; }
`

func TestPeerStateMachine(t *testing.T) {
	parser := protoparse.Parser{Accessor: protoparse.FileContentsFromMap(map[string]string{"projection.proto": schema})}
	files, err := parser.ParseFiles("projection.proto")
	require.NoError(t, err)
	msg := func(name string, fields map[string]any) *dynamic.Message {
		m := dynamic.NewMessage(files[0].FindMessage("projection." + name))
		for k, v := range fields {
			require.NoError(t, m.TrySetFieldByName(k, v))
		}
		return m
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy},
		DockerArgs: map[harness.Component][]string{harness.App: {"-p", "127.0.0.1:" + port + ":1988"}},
		Args:       map[harness.Component][]string{harness.App: {"--cache.distributed_cache.listen_addr=app:1988", "--cache.distributed_cache.nodes=app:1988", "--cache.distributed_cache.cluster_size=1", "--cache.distributed_cache.replication_factor=1", "--cache.distributed_cache.lookaside_cache_size_bytes=0"}},
	})
	peer := harness.Dial(t, addr, f.APIKey)
	public := f.Clients(harness.App)
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "second-state-client", []string{"CACHE_WRITE"})
	second := harness.Dial(t, f.Endpoint(harness.App).GRPC, secondKey)
	proxy := f.Clients(harness.CacheProxy)
	cli, cliErr := exec.LookPath("bb")
	require.NoError(t, cliErr, "bb CLI coverage is required")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "zone", "state-machine-zone")
	{
		publishInvocation(t, ctx, public, "00000000-0000-4000-8000-000000000001")
		result, e := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": "00000000-0000-4000-8000-000000000001"}})
		require.NoError(t, e)
		require.Len(t, result["invocation"], 1)
	}
	call := func(method, request, response string, fields map[string]any) (*dynamic.Message, error) {
		out := msg(response, nil)
		err := peer.Conn.Invoke(ctx, "/distributed_cache.DistributedCache/"+method, msg(request, fields), out)
		return out, err
	}
	for _, scenario := range []struct {
		typ      int32
		instance string
	}{{2, ""}, {1, ""}, {2, "nested/instance"}, {1, "nested/instance"}} {
		typ, instance := scenario.typ, scenario.instance
		t.Run(fmt.Sprintf("cache_type_%d/instance_%s", typ, instance), func(t *testing.T) {
			data, err := proto.Marshal(&repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "target/" + instance}}})
			require.NoError(t, err)
			d := harness.Digest(data)
			if typ == 1 {
				d = harness.Digest([]byte("valid action key"))
				outputDigest, e := public.Upload(ctx, instance, []byte("output file bytes"))
				require.NoError(t, e)
				outputTreeDigest, e := public.UploadProto(ctx, instance, &repb.Tree{Root: &repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "target"}}}})
				require.NoError(t, e)
				data, err = proto.Marshal(&repb.ActionResult{ExitCode: 0, StdoutRaw: []byte("first"), StderrRaw: []byte("initial stderr"), OutputFiles: []*repb.OutputFile{{Path: "out.txt", Digest: outputDigest, IsExecutable: true}}, OutputDirectories: []*repb.OutputDirectory{{Path: "dir", TreeDigest: outputTreeDigest}}, OutputSymlinks: []*repb.OutputSymlink{{Path: "link", Target: "out.txt"}}, ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: "peer-worker", QueuedTimestamp: timestamppb.New(time.Unix(1700000000, 0)), WorkerStartTimestamp: timestamppb.New(time.Unix(1700000001, 0)), WorkerCompletedTimestamp: timestamppb.New(time.Unix(1700000002, 0))}})
				require.NoError(t, err)
			}
			resource := msg("Resource", map[string]any{"digest": msg("Digest", map[string]any{"hash": d.Hash, "size_bytes": d.SizeBytes}), "instance_name": instance, "cache_type": typ, "digest_function": int32(1)})
			var model []byte
			observe := func(assertT require.TestingT) {
				missing, e := call("FindMissing", "MissingRequest", "MissingResponse", map[string]any{"resources": []*dynamic.Message{resource}})
				require.NoError(assertT, e)
				multi, e := call("GetMulti", "MultiRequest", "MultiResponse", map[string]any{"resources": []*dynamic.Message{resource}})
				require.NoError(assertT, e)
				md, e := call("Metadata", "Request", "Metadata", map[string]any{"resource": resource})
				wm, we := call("GetWithMetadata", "WithRequest", "WithResponse", map[string]any{"resource": resource})
				stream, se := peer.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/distributed_cache.DistributedCache/Read")
				require.NoError(assertT, se)
				require.NoError(assertT, stream.SendMsg(msg("ReadRequest", map[string]any{"resource": resource})))
				require.NoError(assertT, stream.CloseSend())
				var got []byte
				for {
					chunk := msg("ReadResponse", nil)
					se = stream.RecvMsg(chunk)
					if se != nil {
						break
					}
					got = append(got, chunk.GetFieldByName("data").([]byte)...)
				}
				ui := msg("UIResponse", nil)
				ue := public.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", msg("UIRequest", map[string]any{"resource_name": resource}), ui)
				httpMD, he := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": map[int32]string{1: "AC", 2: "CAS"}[typ], "digestFunction": "SHA256"}})
				if model == nil {
					wantCount := 1
					require.Len(assertT, missing.GetFieldByName("missing"), wantCount)
					key := missing.GetFieldByName("missing").([]interface{})[0].(*dynamic.Message)
					require.Equal(assertT, d.Hash, key.GetFieldByName("key"))
					require.Equal(assertT, d.SizeBytes, key.GetFieldByName("size_bytes"))
					require.Empty(assertT, multi.GetFieldByName("key_value"))
					for _, err := range []error{e, we, se, ue} {
						require.Equal(assertT, codes.NotFound, status.Code(err), "%v", err)
					}
					require.Error(assertT, he)
					require.Contains(assertT, he.Error(), "code = NotFound")
				} else {
					require.Empty(assertT, missing.GetFieldByName("missing"))
					require.NoError(assertT, e)
					require.NoError(assertT, we)
					require.Equal(assertT, io.EOF, se)
					require.Equal(assertT, model, got)
					require.Equal(assertT, model, wm.GetFieldByName("data"))
					ranged, err := peer.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/distributed_cache.DistributedCache/Read")
					require.NoError(assertT, err)
					require.NoError(assertT, ranged.SendMsg(msg("ReadRequest", map[string]any{"resource": resource, "offset": int64(1), "limit": int64(3)})))
					require.NoError(assertT, ranged.CloseSend())
					var rangeBytes []byte
					for {
						part := msg("ReadResponse", nil)
						err = ranged.RecvMsg(part)
						if err != nil {
							break
						}
						rangeBytes = append(rangeBytes, part.GetFieldByName("data").([]byte)...)
					}
					require.Equal(assertT, io.EOF, err)
					require.Equal(assertT, model[1:4], rangeBytes)

					wmd := wm.GetFieldByName("metadata").(*dynamic.Message)
					for _, name := range []string{"stored_size_bytes", "digest_size_bytes", "last_modify_usec"} {
						require.Equal(assertT, md.GetFieldByName(name), wmd.GetFieldByName(name), name)
					}
					require.Greater(assertT, md.GetFieldByName("last_modify_usec").(int64), int64(0))
					require.Greater(assertT, md.GetFieldByName("last_access_usec").(int64), int64(0))
					require.GreaterOrEqual(assertT, wmd.GetFieldByName("last_access_usec").(int64), md.GetFieldByName("last_access_usec").(int64))
					wantSize := int64(len(model))
					require.Equal(assertT, wantSize, md.GetFieldByName("stored_size_bytes"))
					require.Equal(assertT, d.SizeBytes, md.GetFieldByName("digest_size_bytes"))
					require.NoError(assertT, ue)
					require.Equal(assertT, md.GetFieldByName("stored_size_bytes"), ui.GetFieldByName("stored_size_bytes"))
					require.NoError(assertT, he)
					require.Equal(assertT, d.SizeBytes, ui.GetFieldByName("digest_size_bytes"))
					require.Equal(assertT, fmt.Sprint(d.SizeBytes), fmt.Sprint(httpMD["digestSizeBytes"]))
					require.Equal(assertT, fmt.Sprint(md.GetFieldByName("last_modify_usec")), fmt.Sprint(httpMD["lastModifyUsec"]))
					require.Equal(assertT, md.GetFieldByName("last_modify_usec"), ui.GetFieldByName("last_modify_usec"))
					require.GreaterOrEqual(assertT, ui.GetFieldByName("last_access_usec").(int64), md.GetFieldByName("last_access_usec").(int64))
					access, e := strconv.ParseInt(fmt.Sprint(httpMD["lastAccessUsec"]), 10, 64)
					require.NoError(assertT, e)
					require.GreaterOrEqual(assertT, access, md.GetFieldByName("last_access_usec").(int64))
					require.LessOrEqual(assertT, access, time.Now().UnixMicro())
					require.LessOrEqual(assertT, md.GetFieldByName("last_modify_usec").(int64), time.Now().UnixMicro())
					require.Equal(assertT, fmt.Sprint(len(model)), fmt.Sprint(httpMD["storedSizeBytes"]))
					kv := multi.GetFieldByName("key_value").([]interface{})
					require.Len(assertT, kv, 1)
					key := kv[0].(*dynamic.Message).GetFieldByName("key").(*dynamic.Message)
					require.Equal(assertT, d.Hash, key.GetFieldByName("key"))
					require.Equal(assertT, d.SizeBytes, key.GetFieldByName("size_bytes"))
					require.Equal(assertT, model, kv[0].(*dynamic.Message).GetFieldByName("value"))
				}

				{
					scheme, path := "bytestream", harness.ResourceName(instance, d, false)
					if typ == 1 {
						scheme = "actioncache"
						path = fmt.Sprintf("%sblobs/ac/%s/%d", func() string {
							if instance == "" {
								return ""
							}
							return instance + "/"
						}(), d.Hash, d.SizeBytes)
					}
					for _, view := range []string{"download", "view"} {
						endpoint := f.Web.URL + "/file/" + view + "?invocation_id=00000000-0000-4000-8000-000000000001&bytestream_url=" + url.QueryEscape(scheme+"://app:1985/"+path)
						req, e := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
						require.NoError(assertT, e)
						req.Header.Set("x-buildbuddy-api-key", f.APIKey)
						rsp, e := f.Web.Client.Do(req)
						require.NoError(assertT, e)
						body, e := io.ReadAll(rsp.Body)
						require.NoError(assertT, e)
						require.NoError(assertT, rsp.Body.Close())
						// Exact status and bytes below cover both UI handlers.
						if model == nil {
							require.Equal(assertT, http.StatusNotFound, rsp.StatusCode)
						} else {
							require.Equal(assertT, http.StatusOK, rsp.StatusCode, "%s", body)
							require.Equal(assertT, model, body)
						}
					}
				}
				if typ == 2 {

					for index, client := range []*harness.Clients{public, proxy} {
						for _, compressed := range []bool{false, true} {
							fields := map[string]any{"resource_name": harness.ResourceName(instance, d, compressed)}
							if !compressed {
								fields["read_offset"] = int64(1)
								fields["read_limit"] = int64(3)
							}
							stream, err := client.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/google.bytestream.ByteStream/Read")
							require.NoError(assertT, err)
							require.NoError(assertT, stream.SendMsg(msg("BSRead", fields)))
							require.NoError(assertT, stream.CloseSend())
							var output []byte
							for {
								chunk := msg("BSResponse", nil)
								err = stream.RecvMsg(chunk)
								if err != nil {
									break
								}
								output = append(output, chunk.GetFieldByName("data").([]byte)...)
							}
							if model == nil && (index == 0 || err != io.EOF) {
								require.Equal(assertT, codes.NotFound, status.Code(err))
								continue
							}
							require.Equal(assertT, io.EOF, err)
							want := model
							if want == nil {
								want = data
							}
							if compressed {
								output, err = harness.Decompress(output)
								require.NoError(assertT, err)
							} else {
								want = want[1:4]
							}
							require.Equal(assertT, want, output)
						}
					}
					uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, false)
					// Public file APIs expose the same byte register, including nonzero ranges.
					rangeOut := msg("ReadResponse", nil)
					rangeErr := public.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", msg("FileRequest", map[string]any{"uri": uri, "start": int64(1), "end": int64(4)}), rangeOut)
					if model == nil {
						require.Equal(assertT, codes.NotFound, status.Code(rangeErr))
					} else {
						require.NoError(assertT, rangeErr)
						require.Equal(assertT, model[1:4], rangeOut.GetFieldByName("data"))
					}
					for _, conn := range []*grpc.ClientConn{public.Conn, second.Conn} {
						stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
						require.NoError(assertT, err)
						require.NoError(assertT, stream.SendMsg(msg("FileRequest", map[string]any{"uri": uri})))
						require.NoError(assertT, stream.CloseSend())
						var bytesOut []byte
						for {
							chunk := msg("ReadResponse", nil)
							err = stream.RecvMsg(chunk)
							if err != nil {
								break
							}
							bytesOut = append(bytesOut, chunk.GetFieldByName("data").([]byte)...)
						}
						if model == nil {
							require.Equal(assertT, codes.Unavailable, status.Code(err))
						} else {
							require.Equal(assertT, io.EOF, err)
							require.Equal(assertT, model, bytesOut)
						}
					}
					body, _ := json.Marshal(map[string]any{"uri": uri, "start": "1", "end": "4"})
					request, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/GetFileRange", bytes.NewReader(body))
					require.NoError(assertT, err)
					request.Header.Set("x-buildbuddy-api-key", f.APIKey)
					request.Header.Set("Content-Type", "application/json")
					response, err := f.Web.Client.Do(request)
					require.NoError(assertT, err)
					raw, err := io.ReadAll(response.Body)
					require.NoError(assertT, err)
					require.NoError(assertT, response.Body.Close())
					if model == nil {
						require.Contains(assertT, string(raw), "NotFound")
					} else {
						require.Equal(assertT, 200, response.StatusCode, string(raw))
						var result map[string]any
						require.NoError(assertT, json.Unmarshal(raw, &result))
						require.Equal(assertT, base64.StdEncoding.EncodeToString(model[1:4]), result["data"])
					}
					// A valid leaf Directory makes GetTree a current-state oracle rather than
					// attempting to parse arbitrary blob bytes as a Directory.
					for _, client := range []*harness.Clients{public, proxy} {
						tree, err := client.CAS.GetTree(ctx, &repb.GetTreeRequest{RootDigest: d, InstanceName: instance})
						require.NoError(assertT, err)
						rsp, err := tree.Recv()
						if model == nil {
							require.Equal(assertT, codes.FailedPrecondition, status.Code(err))
						} else {
							require.NoError(assertT, err)
							require.Len(assertT, rsp.Directories, 1)
							want := &repb.Directory{}
							require.NoError(assertT, proto.Unmarshal(model, want))
							require.True(assertT, proto.Equal(want, rsp.Directories[0]))
							_, err = tree.Recv()
							require.Equal(assertT, io.EOF, err)
						}
					}
					treeRequest := map[string]any{"instance_name": instance, "root_digest": msg("Digest", map[string]any{"hash": d.Hash, "size_bytes": d.SizeBytes})}
					uiTree := msg("UITreeResponse", nil)
					treeErr := public.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTree", msg("UITreeRequest", treeRequest), uiTree)
					sizes := msg("Sizes", nil)
					sizeErr := public.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", msg("SizeRequest", treeRequest), sizes)
					jsonSizes, jsonErr := f.Web.RPC("GetTreeDirectorySizes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "instanceName": instance, "rootDigest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}})
					if model == nil {
						require.Equal(assertT, codes.FailedPrecondition, status.Code(treeErr))
						require.Equal(assertT, codes.FailedPrecondition, status.Code(sizeErr))
						require.Error(assertT, jsonErr)
						require.Contains(assertT, jsonErr.Error(), "FailedPrecondition")
					} else {
						require.NoError(assertT, treeErr)
						require.Len(assertT, uiTree.GetFieldByName("directories"), 1)
						directory := uiTree.GetFieldByName("directories").([]interface{})[0].(*dynamic.Message)
						symlinks := directory.GetFieldByName("symlinks").([]interface{})
						require.Len(assertT, symlinks, 1)
						require.Equal(assertT, "link", symlinks[0].(*dynamic.Message).GetFieldByName("name"))
						require.Equal(assertT, "target/"+instance, symlinks[0].(*dynamic.Message).GetFieldByName("target"))
						require.NoError(assertT, sizeErr)
						require.Len(assertT, sizes.GetFieldByName("sizes"), 1)
						size := sizes.GetFieldByName("sizes").([]interface{})[0].(*dynamic.Message)
						require.Equal(assertT, d.Hash, size.GetFieldByName("digest"))
						require.Equal(assertT, d.SizeBytes, size.GetFieldByName("total_size"))
						require.Equal(assertT, int64(0), size.GetFieldByName("child_count"))
						require.NoError(assertT, jsonErr)
						values := jsonSizes["sizes"].([]interface{})
						require.Len(assertT, values, 1)
						require.Equal(assertT, d.Hash, values[0].(map[string]any)["digest"])
						require.Equal(assertT, fmt.Sprint(d.SizeBytes), fmt.Sprint(values[0].(map[string]any)["totalSize"]))
					}

					// Match the browser transport, including streamed data frames and the
					// terminal gRPC status frame, instead of treating HTTP 200 as success.
					for _, surface := range []struct {
						method, request, response string
						fields                    map[string]any
						missing                   codes.Code
					}{{"GetTree", "UITreeRequest", "UITreeResponse", treeRequest, codes.FailedPrecondition}, {"GetTreeDirectorySizes", "SizeRequest", "Sizes", treeRequest, codes.FailedPrecondition}, {"GetCacheMetadata", "UIRequest", "UIResponse", map[string]any{"resource_name": resource}, codes.NotFound}} {
						payload, err := msg(surface.request, surface.fields).Marshal()
						require.NoError(assertT, err)
						frame := make([]byte, 5)
						binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
						frame = append(frame, payload...)
						req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+surface.method, bytes.NewReader(frame))
						require.NoError(assertT, err)
						req.Header.Set("Content-Type", "application/proto+prefixed")
						req.Header.Set("x-buildbuddy-api-key", f.APIKey)
						rsp, err := f.Web.Client.Do(req)
						require.NoError(assertT, err)
						raw, err := io.ReadAll(rsp.Body)
						require.NoError(assertT, err)
						require.NoError(assertT, rsp.Body.Close())
						require.Equal(assertT, 200, rsp.StatusCode)
						code := rsp.Header.Get("Grpc-Status")
						var responses []*dynamic.Message
						for len(raw) > 0 {
							require.GreaterOrEqual(assertT, len(raw), 5)
							flags := raw[0]
							n := int(binary.BigEndian.Uint32(raw[1:5]))
							raw = raw[5:]
							require.GreaterOrEqual(assertT, len(raw), n)
							payload := raw[:n]
							raw = raw[n:]
							if flags == 0x80 {
								for _, line := range strings.Split(string(payload), "\r\n") {
									if strings.HasPrefix(strings.ToLower(line), "grpc-status:") {
										code = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
									}
								}
							} else {
								require.Equal(assertT, byte(0), flags)
								m := msg(surface.response, nil)
								require.NoError(assertT, m.Unmarshal(payload))
								responses = append(responses, m)
							}
						}
						if model == nil {
							require.Equal(assertT, fmt.Sprint(int(surface.missing)), code)
							require.Empty(assertT, responses)
						} else {
							require.Equal(assertT, "0", code)
							require.Len(assertT, responses, 1)
							switch surface.method {
							case "GetCacheMetadata":
								require.Equal(assertT, d.SizeBytes, responses[0].GetFieldByName("digest_size_bytes"))
								require.Equal(assertT, int64(len(model)), responses[0].GetFieldByName("stored_size_bytes"))
								require.Equal(assertT, md.GetFieldByName("last_modify_usec"), responses[0].GetFieldByName("last_modify_usec"))
								require.GreaterOrEqual(assertT, responses[0].GetFieldByName("last_access_usec").(int64), md.GetFieldByName("last_access_usec").(int64))
							case "GetTree":
								require.Len(assertT, responses[0].GetFieldByName("directories"), 1)
								directory := responses[0].GetFieldByName("directories").([]interface{})[0].(*dynamic.Message)
								wire, err := directory.Marshal()
								require.NoError(assertT, err)
								gotDir, wantDir := &repb.Directory{}, &repb.Directory{}
								require.NoError(assertT, proto.Unmarshal(wire, gotDir))
								require.NoError(assertT, proto.Unmarshal(model, wantDir))
								require.True(assertT, proto.Equal(wantDir, gotDir))
							case "GetTreeDirectorySizes":
								require.Len(assertT, responses[0].GetFieldByName("sizes"), 1)
								size := responses[0].GetFieldByName("sizes").([]interface{})[0].(*dynamic.Message)
								require.Equal(assertT, d.Hash, size.GetFieldByName("digest"))
								require.Equal(assertT, d.SizeBytes, size.GetFieldByName("total_size"))
								require.Equal(assertT, int64(0), size.GetFieldByName("child_count"))
							}
						}
					}

					if cliErr == nil {
						output, e := exec.CommandContext(ctx, cli, "download", harness.ResourceName(instance, d, false), "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey).CombinedOutput()
						if model == nil {
							require.Error(assertT, e)
							require.Contains(assertT, string(output), "NotFound")
						} else {
							require.NoError(assertT, e)
							require.Equal(assertT, model, output)
						}
					}
					proxyOutput, proxyCLIError := exec.CommandContext(ctx, cli, "download", harness.ResourceName(instance, d, false), "--target=grpc://"+f.Endpoint(harness.CacheProxy).GRPC, "--api_key="+secondKey).CombinedOutput()
					if model != nil {
						require.NoError(assertT, proxyCLIError)
						require.Equal(assertT, model, proxyOutput)
					} else if proxyCLIError != nil {
						require.Contains(assertT, string(proxyOutput), "NotFound")
					} else {
						require.Equal(assertT, data, proxyOutput)
					}
					// Proxy CAS is immutable read-through storage: upstream deletion does
					// not invalidate a cached copy. Existence queries remain authoritative.
					pf, pe := proxy.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
					require.NoError(assertT, pe)
					pb, pe := proxy.Read(ctx, instance, d)
					if model != nil {
						require.Empty(assertT, pf.MissingBlobDigests)
						require.NoError(assertT, pe)
						require.Equal(assertT, model, pb)
					} else {
						require.Len(assertT, pf.MissingBlobDigests, 1)
						require.True(assertT, proto.Equal(d, pf.MissingBlobDigests[0]))
						if pe == nil {
							require.Equal(assertT, data, pb)
						} else {
							require.Equal(assertT, codes.NotFound, status.Code(pe))
						}
					}
					pbatch, pe := proxy.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
					require.NoError(assertT, pe)
					require.Len(assertT, pbatch.Responses, 1)
					require.True(assertT, proto.Equal(d, pbatch.Responses[0].Digest))
					if pbatch.Responses[0].Status.Code == 0 {
						require.Equal(assertT, data, pbatch.Responses[0].Data)
					} else {
						require.Nil(assertT, model)
						require.Equal(assertT, int32(codes.NotFound), pbatch.Responses[0].Status.Code)
					}
					fm, e := public.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
					require.NoError(assertT, e)
					batch, e := public.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
					require.NoError(assertT, e)
					require.Len(assertT, batch.Responses, 1)
					require.True(assertT, proto.Equal(d, batch.Responses[0].Digest))
					b, e := public.Read(ctx, instance, d)
					if model == nil {
						require.Len(assertT, fm.MissingBlobDigests, 1)
						require.True(assertT, proto.Equal(d, fm.MissingBlobDigests[0]))
						require.Equal(assertT, int32(codes.NotFound), batch.Responses[0].Status.Code)
						require.Equal(assertT, codes.NotFound, status.Code(e))
					} else {
						require.Empty(assertT, fm.MissingBlobDigests)
						require.NoError(assertT, e)
						require.Equal(assertT, model, b)
						require.Equal(assertT, int32(codes.OK), batch.Responses[0].Status.Code)
						require.True(assertT, proto.Equal(d, batch.Responses[0].Digest))
						require.Equal(assertT, model, batch.Responses[0].Data)
					}

				} else {
					uri := fmt.Sprintf("actioncache://app:1985/%sblobs/ac/%s/%d", func() string {
						if instance == "" {
							return ""
						}
						return instance + "/"
					}(), d.Hash, d.SizeBytes)
					stream, err := public.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
					require.NoError(assertT, err)
					require.NoError(assertT, stream.SendMsg(msg("FileRequest", map[string]any{"uri": uri})))
					require.NoError(assertT, stream.CloseSend())
					var raw []byte
					for {
						chunk := msg("ReadResponse", nil)
						err = stream.RecvMsg(chunk)
						if err != nil {
							break
						}
						raw = append(raw, chunk.GetFieldByName("data").([]byte)...)
					}
					if model == nil {
						require.Equal(assertT, codes.Unavailable, status.Code(err))
					} else {
						require.Equal(assertT, io.EOF, err)
						require.Equal(assertT, model, raw)
					}
					par, pe := proxy.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d})
					if model == nil {
						require.Equal(assertT, codes.NotFound, status.Code(pe))
					} else {
						require.NoError(assertT, pe)
						want := &repb.ActionResult{}
						require.NoError(assertT, proto.Unmarshal(model, want))
						require.True(assertT, proto.Equal(want, par), "full ActionResult: %v", par)
					}
					ar, e := public.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d})
					if model == nil {
						require.Equal(assertT, codes.NotFound, status.Code(e))
					} else {
						require.NoError(assertT, e)
						want := &repb.ActionResult{}
						require.NoError(assertT, proto.Unmarshal(model, want))
						require.True(assertT, proto.Equal(want, ar), "full ActionResult: %v", ar)
					}
				}
			}
			check := func() {
				started := time.Now()
				require.EventuallyWithT(t, func(collect *assert.CollectT) { observe(collect) }, 10*time.Second, 100*time.Millisecond)
				elapsed := time.Since(started)
				time.Sleep(100 * time.Millisecond)
				observe(t)
				t.Logf("all modeled projections observed within %s; stable at +100ms (documented proxy retention excluded from agreement)", elapsed)
			}

			write := func(b []byte) {
				s, e := peer.Conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, "/distributed_cache.DistributedCache/Write")
				require.NoError(t, e)
				for i, part := range [][]byte{b[:len(b)/2], b[len(b)/2:]} {
					require.NoError(t, s.SendMsg(msg("WriteRequest", map[string]any{"resource": resource, "data": part, "finish_write": i == 1})))
				}
				require.NoError(t, s.CloseSend())
				out := msg("WriteResponse", nil)
				require.NoError(t, s.RecvMsg(out))
				require.Equal(t, int64(len(b)), out.GetFieldByName("committed_size"))
				require.Equal(t, io.EOF, s.RecvMsg(msg("WriteResponse", nil)))
				model = bytes.Clone(b)
				check()
			}
			// Public services can become reachable shortly after fixture readiness.
			require.Eventually(t, func() bool {
				out := msg("UIResponse", nil)
				e := public.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", msg("UIRequest", map[string]any{"resource_name": resource}), out)
				return status.Code(e) == codes.NotFound
			}, 10*time.Second, 100*time.Millisecond)
			check()
			write(data)
			write(data)
			if typ == 1 {
				replacement, e := proto.Marshal(&repb.ActionResult{ExitCode: 7, StdoutRaw: []byte("replacement")})
				require.NoError(t, e)
				write(replacement)
			}

			for index, client := range []*harness.Clients{proxy, second, public} {
				started := time.Now()
				if typ == 2 {
					uploaded, e := client.Upload(ctx, instance, data)
					require.NoError(t, e)
					require.True(t, proto.Equal(d, uploaded))
					model = bytes.Clone(data)
				} else {
					result, e := client.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, ActionResult: &repb.ActionResult{ExitCode: int32(index + 3), StdoutRaw: []byte("public update"), StderrRaw: []byte("stderr"), ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: "deterministic-worker"}}})
					require.NoError(t, e)
					model, e = proto.Marshal(result)
					require.NoError(t, e)
				}
				check()
				elapsed := time.Since(started)
				time.Sleep(100 * time.Millisecond)
				check()
				t.Logf("public mutation all checked surfaces agree by %s and remain stable after 100ms", elapsed)
			}
			for i := 0; i < 2; i++ {
				_, e := call("Heartbeat", "Heartbeat", "Empty", map[string]any{"source": "app:1988"})
				require.NoError(t, e)
				check()
			}
			for i := 0; i < 2; i++ {
				_, e := call("Delete", "Request", "Empty", map[string]any{"resource": resource})
				if i == 0 {
					require.NoError(t, e)
				} else {
					require.True(t, e == nil || status.Code(e) == codes.NotFound, "repeat delete: %v", e)
				}
				model = nil
				check()
			}
			write(data)
		})
	}
}

// Minimal BES fixture for the download endpoint artifact fallback.
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

// This characterizes a known cross-surface divergence rather than accepting
// stale data in an eventual-consistency assertion. It passes only while the
// documented divergence exists, and fails when the release behavior changes.
func TestProxyDeleteRetention(t *testing.T)   { testProxyDeleteRetention(t, false) }
func TestProxyDeleteConformance(t *testing.T) { testProxyDeleteRetention(t, true) }
func testProxyDeleteRetention(t *testing.T, conformance bool) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	data := []byte("isolated deletion retention")
	d, err := app.Upload(ctx, "retention", data)
	require.NoError(t, err)
	b, err := proxy.Read(ctx, "retention", d)
	require.NoError(t, err)
	require.Equal(t, data, b)
	parser := protoparse.Parser{Accessor: protoparse.FileContentsFromMap(map[string]string{"p.proto": `syntax="proto3";package p;message Request {string uri=1;} message Empty {}`})}
	files, err := parser.ParseFiles("p.proto")
	require.NoError(t, err)
	req := dynamic.NewMessage(files[0].FindMessage("p.Request"))
	req.SetFieldByName("uri", harness.ResourceName("retention", d, false))
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", req, dynamic.NewMessage(files[0].FindMessage("p.Empty"))))
	start := time.Now()
	for i := 0; i <= 10; i++ {
		fm, err := proxy.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "retention", BlobDigests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, fm.MissingBlobDigests, 1)
		require.True(t, proto.Equal(d, fm.MissingBlobDigests[0]))
		_, err = app.Read(ctx, "retention", d)
		require.Equal(t, codes.NotFound, status.Code(err))
		b, err = proxy.Read(ctx, "retention", d)
		if conformance {
			require.Eventually(t, func() bool { _, e := proxy.Read(ctx, "retention", d); return status.Code(e) == codes.NotFound }, 10*time.Second, 100*time.Millisecond, "proxy reads deleted bytes while FindMissing reports absent")
			time.Sleep(100 * time.Millisecond)
			_, err = proxy.Read(ctx, "retention", d)
			require.Equal(t, codes.NotFound, status.Code(err))
			return
		}
		require.NoError(t, err)
		require.Equal(t, data, b)
		batch, err := proxy.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "retention", Digests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, batch.Responses, 1)
		require.Equal(t, int32(codes.OK), batch.Responses[0].Status.Code)
		require.Equal(t, data, batch.Responses[0].Data)
		if i < 10 {
			time.Sleep(time.Second)
		}
	}
	t.Logf("DIVERGENCE: app missing, proxy existence missing, proxy ByteStream/BatchRead present; stable for %s; no convergence within 10s bound", time.Since(start))
}

// Two released cache-bearing components form a static replication-factor-two
// ring. Public mutations, unlike peer-local RPCs, own replica fanout.
func TestReplicatedPublicSequence(t *testing.T) {
	addresses := make([]string, 2)
	ports := make([]string, 2)
	for i := range addresses {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, e)
		addresses[i] = l.Addr().String()
		_, ports[i], e = net.SplitHostPort(addresses[i])
		require.NoError(t, e)
		require.NoError(t, l.Close())
	}
	common := []string{"--cache.distributed_cache.nodes=app:1988,cacheproxy:1988", "--cache.distributed_cache.cluster_size=2", "--cache.distributed_cache.replication_factor=2", "--cache.distributed_cache.lookaside_cache_size_bytes=0"}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, DockerArgs: map[harness.Component][]string{harness.App: {"-p", "127.0.0.1:" + ports[0] + ":1988"}, harness.CacheProxy: {"-p", "127.0.0.1:" + ports[1] + ":1988"}}, Args: map[harness.Component][]string{harness.App: append(append([]string{}, common...), "--cache.distributed_cache.listen_addr=app:1988", "--enable_cache_delete_api=true"), harness.CacheProxy: append(append([]string{}, common...), "--cache.distributed_cache.listen_addr=cacheproxy:1988", "--auth.remote.use_es256_jwts=false", "--auth.jwt_key=fixture-only-hmac-key", "--auth.reparse_jwts=true")}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	parser := protoparse.Parser{Accessor: protoparse.FileContentsFromMap(map[string]string{"p.proto": schema})}
	files, e := parser.ParseFiles("p.proto")
	require.NoError(t, e)
	msg := func(name string, fields map[string]any) *dynamic.Message {
		m := dynamic.NewMessage(files[0].FindMessage("projection." + name))
		for k, v := range fields {
			require.NoError(t, m.TrySetFieldByName(k, v))
		}
		return m
	}
	peers := []*harness.Clients{harness.Dial(t, addresses[0], f.APIKey), harness.Dial(t, addresses[1], f.APIKey)}
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	token := msg("AuthResponse", nil)
	require.NoError(t, app.Conn.Invoke(ctx, "/auth.AuthService/Authenticate", msg("AuthRequest", map[string]any{"jwt_signing_method": int32(1)}), token))
	ctx = metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-jwt", token.GetFieldByName("jwt").(string))
	data := []byte("replicated state sequence")
	d := harness.Digest(data)
	resource := msg("Resource", map[string]any{"digest": msg("Digest", map[string]any{"hash": d.Hash, "size_bytes": d.SizeBytes}), "instance_name": "replicated", "cache_type": int32(2), "digest_function": int32(1)})
	present := false
	observeReplicas := func(assertT require.TestingT) {
		for _, peer := range peers {
			out := msg("WithResponse", nil)
			err := peer.Conn.Invoke(ctx, "/distributed_cache.DistributedCache/GetWithMetadata", msg("WithRequest", map[string]any{"resource": resource}), out)
			if present {
				require.NoError(assertT, err)
				require.Equal(assertT, data, out.GetFieldByName("data"))
				md := out.GetFieldByName("metadata").(*dynamic.Message)
				require.Equal(assertT, d.SizeBytes, md.GetFieldByName("digest_size_bytes"))
				require.Equal(assertT, d.SizeBytes, md.GetFieldByName("stored_size_bytes"))
			} else {
				require.Equal(assertT, codes.NotFound, status.Code(err), "peer/read error: %v", err)
			}
		}
		for _, client := range []*harness.Clients{app, proxy} {
			fm, err := client.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "replicated", BlobDigests: []*repb.Digest{d}})
			require.NoError(assertT, err)
			b, err := client.Read(ctx, "replicated", d)
			if present {
				require.Empty(assertT, fm.MissingBlobDigests)
				require.NoError(assertT, err)
				require.Equal(assertT, data, b)
			} else {
				require.Len(assertT, fm.MissingBlobDigests, 1)
				require.True(assertT, proto.Equal(d, fm.MissingBlobDigests[0]))
				require.Equal(assertT, codes.NotFound, status.Code(err), "peer/read error: %v", err)
			}
		}
	}
	check := func() {
		started := time.Now()
		require.EventuallyWithT(t, func(collect *assert.CollectT) { observeReplicas(collect) }, 10*time.Second, 100*time.Millisecond)
		elapsed := time.Since(started)
		time.Sleep(100 * time.Millisecond)
		observeReplicas(t)
		t.Logf("replica agreement by %s; stable at +100ms", elapsed)
	}

	check()
	for step, client := range []*harness.Clients{proxy, app, proxy} {
		started := time.Now()
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			uploaded, err := client.Upload(ctx, "replicated", data)
			require.NoError(collect, err)
			require.True(collect, proto.Equal(d, uploaded))
		}, 10*time.Second, 100*time.Millisecond)
		present = true
		check()
		time.Sleep(100 * time.Millisecond)
		check()
		t.Logf("replicated write step=%d stable agreement observed within %s", step, time.Since(started))
	}
	request := msg("FileRequest", map[string]any{"uri": harness.ResourceName("replicated", d, false)})
	require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", request, msg("Empty", nil)))
	present = false
	check()
	time.Sleep(100 * time.Millisecond)
	check()
	_, e = proxy.Upload(ctx, "replicated", data)
	require.NoError(t, e)
	present = true
	check()
	for index, peer := range peers {
		hbctx := metadata.AppendToOutgoingContext(ctx, "zone", fmt.Sprintf("zone-%d", index))
		require.NoError(t, peer.Conn.Invoke(hbctx, "/distributed_cache.DistributedCache/Heartbeat", msg("Heartbeat", map[string]any{"source": []string{"app:1988", "cacheproxy:1988"}[index]}), msg("Empty", nil)))
		check()
	}
}
