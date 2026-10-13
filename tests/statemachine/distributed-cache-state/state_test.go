package distributedcache_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/jhump/protoreflect/desc/protoparse"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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
message UIResponse { int64 stored_size_bytes=2; int64 digest_size_bytes=5; }
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
	proxy := f.Clients(harness.CacheProxy)
	cli, cliErr := exec.LookPath("bb")
	if cliErr != nil {
		t.Log("bb download unavailable: runner has no bb executable; ByteStream checked directly")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	call := func(method, request, response string, fields map[string]any) (*dynamic.Message, error) {
		out := msg(response, nil)
		err := peer.Conn.Invoke(ctx, "/distributed_cache.DistributedCache/"+method, msg(request, fields), out)
		return out, err
	}
	for _, typ := range []int32{2, 1} {
		t.Run(fmt.Sprintf("cache_type_%d", typ), func(t *testing.T) {
			data := []byte("peer CAS state")
			d := harness.Digest(data)
			if typ == 1 {
				d = harness.Digest([]byte("valid action key"))
				data, err = proto.Marshal(&repb.ActionResult{ExitCode: 0, StdoutRaw: []byte("first")})
				require.NoError(t, err)
			}
			resource := msg("Resource", map[string]any{"digest": msg("Digest", map[string]any{"hash": d.Hash, "size_bytes": d.SizeBytes}), "instance_name": "", "cache_type": typ, "digest_function": int32(1)})
			var model []byte
			check := func() {
				missing, e := call("FindMissing", "MissingRequest", "MissingResponse", map[string]any{"resources": []*dynamic.Message{resource}})
				require.NoError(t, e)
				multi, e := call("GetMulti", "MultiRequest", "MultiResponse", map[string]any{"resources": []*dynamic.Message{resource}})
				require.NoError(t, e)
				md, e := call("Metadata", "Request", "Metadata", map[string]any{"resource": resource})
				wm, we := call("GetWithMetadata", "WithRequest", "WithResponse", map[string]any{"resource": resource})
				stream, se := peer.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/distributed_cache.DistributedCache/Read")
				require.NoError(t, se)
				require.NoError(t, stream.SendMsg(msg("ReadRequest", map[string]any{"resource": resource})))
				require.NoError(t, stream.CloseSend())
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
				httpMD, he := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": "", "cacheType": map[int32]string{1: "AC", 2: "CAS"}[typ], "digestFunction": "SHA256"}})
				if model == nil {
					require.Len(t, missing.GetFieldByName("missing"), 1)
					require.Empty(t, multi.GetFieldByName("key_value"))
					for _, err := range []error{e, we, se, ue} {
						require.Equal(t, codes.NotFound, status.Code(err), "%v", err)
					}
					require.Error(t, he)
				} else {
					require.Empty(t, missing.GetFieldByName("missing"))
					require.NoError(t, e)
					require.NoError(t, we)
					require.Equal(t, io.EOF, se)
					require.Equal(t, model, got)
					require.Equal(t, model, wm.GetFieldByName("data"))
					require.Equal(t, int64(len(model)), md.GetFieldByName("stored_size_bytes"))
					require.Equal(t, d.SizeBytes, md.GetFieldByName("digest_size_bytes"))
					require.NoError(t, ue)
					require.Equal(t, md.GetFieldByName("stored_size_bytes"), ui.GetFieldByName("stored_size_bytes"))
					require.NoError(t, he)
					require.Equal(t, fmt.Sprint(len(model)), fmt.Sprint(httpMD["storedSizeBytes"]))
					kv := multi.GetFieldByName("key_value").([]interface{})
					require.Len(t, kv, 1)
					require.Equal(t, model, kv[0].(*dynamic.Message).GetFieldByName("value"))
				}
				if typ == 2 {
					if cliErr == nil {
						output, e := exec.CommandContext(ctx, cli, "download", fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes), "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey).Output()
						if model == nil {
							require.Error(t, e)
						} else {
							require.NoError(t, e)
							require.Equal(t, model, output)
						}
					}
					// Proxy CAS is immutable read-through storage: upstream deletion does
					// not invalidate a cached copy. Existence queries remain authoritative.
					pf, pe := proxy.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "", BlobDigests: []*repb.Digest{d}})
					require.NoError(t, pe)
					pb, pe := proxy.Read(ctx, "", d)
					if model != nil {
						require.Empty(t, pf.MissingBlobDigests)
						require.NoError(t, pe)
						require.Equal(t, model, pb)
					} else {
						require.Len(t, pf.MissingBlobDigests, 1)
						if pe == nil {
							require.Equal(t, data, pb)
						} else {
							require.Equal(t, codes.NotFound, status.Code(pe))
						}
					}
					pbatch, pe := proxy.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "", Digests: []*repb.Digest{d}})
					require.NoError(t, pe)
					require.Len(t, pbatch.Responses, 1)
					if pbatch.Responses[0].Status.Code == 0 {
						require.Equal(t, data, pbatch.Responses[0].Data)
					} else {
						require.Nil(t, model)
						require.Equal(t, int32(codes.NotFound), pbatch.Responses[0].Status.Code)
					}
					fm, e := public.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "", BlobDigests: []*repb.Digest{d}})
					require.NoError(t, e)
					batch, e := public.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "", Digests: []*repb.Digest{d}})
					require.NoError(t, e)
					require.Len(t, batch.Responses, 1)
					b, e := public.Read(ctx, "", d)
					if model == nil {
						require.Len(t, fm.MissingBlobDigests, 1)
						require.Equal(t, int32(codes.NotFound), batch.Responses[0].Status.Code)
						require.Equal(t, codes.NotFound, status.Code(e))
					} else {
						require.Empty(t, fm.MissingBlobDigests)
						require.NoError(t, e)
						require.Equal(t, model, b)
						require.Equal(t, model, batch.Responses[0].Data)
					}
				} else {
					par, pe := proxy.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: "", ActionDigest: d})
					if model == nil {
						require.Equal(t, codes.NotFound, status.Code(pe))
					} else {
						require.NoError(t, pe)
						want := &repb.ActionResult{}
						require.NoError(t, proto.Unmarshal(model, want))
						require.Equal(t, want.ExitCode, par.ExitCode)
						require.Equal(t, want.StdoutRaw, par.StdoutRaw)
					}
					ar, e := public.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: "", ActionDigest: d})
					if model == nil {
						require.Equal(t, codes.NotFound, status.Code(e))
					} else {
						require.NoError(t, e)
						want := &repb.ActionResult{}
						require.NoError(t, proto.Unmarshal(model, want))
						require.Equal(t, want.ExitCode, ar.ExitCode)
						require.Equal(t, want.StdoutRaw, ar.StdoutRaw)
					}
				}
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
