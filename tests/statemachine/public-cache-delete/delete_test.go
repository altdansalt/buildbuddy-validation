package public_cache_delete_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
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
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	invocationID := "b7b751bd-2619-40ba-ad97-201e89296c22"
	publishInvocation(t, ctx, c, invocationID)
	invoke := func(method string, b []byte) (wire, error) {
		req := wire(b)
		var res wire
		err := c.Conn.Invoke(ctx, method, &req, &res, grpc.ForceCodec(codec{}))
		return res, err
	}
	httpCall := func(method string, req any) (int, []byte) {
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
	checkFile := func(uri string, present bool, expected []byte) {
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
		code, out := httpCall("GetFile", map[string]any{"uri": uri})
		if present {
			require.Equal(t, 200, code)
			require.Equal(t, expected, out)
		} else {
			require.Equal(t, 404, code)
		}
		r, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?invocation_id="+invocationID+"&bytestream_url="+url.QueryEscape(uri), nil)
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

	for _, instance := range []string{"", "ci/nested"} {
		t.Run(fmt.Sprintf("instance=%s", instance), func(t *testing.T) {
			blob := []byte("immutable output for " + instance)
			d, err := c.Upload(ctx, instance, blob)
			require.NoError(t, err)
			other := []byte("unrelated blob " + instance)
			od, err := c.Upload(ctx, instance, other)
			require.NoError(t, err)
			root, err := c.UploadProto(ctx, instance, &repb.Directory{})
			require.NoError(t, err)
			var keys []*repb.Digest
			for _, arg := range []string{"first", "second"} {
				command, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"sh", "-c", "printf " + arg}, OutputPaths: []string{"out.txt"}})
				require.NoError(t, err)
				action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: command, InputRootDigest: root})
				require.NoError(t, err)
				keys = append(keys, action)
			}
			model := map[string]*repb.ActionResult{}
			casPresent := true
			resource := harness.ResourceName(instance, d, false)
			uri := "bytestream://" + f.Endpoint(harness.App).InternalGRPC[len("grpc://"):] + "/" + resource
			metadataCheck := func(d *repb.Digest, ac, present bool) {
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
					_, err = invoke("/buildbuddy.service.BuildBuddyService/GetCacheMetadata", field(2, rn))
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
					require.NotEmpty(t, res)
				} else {
					require.Error(t, err)
					require.Contains(t, err.Error(), "NotFound")
				}
			}
			check := func() {
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
				for _, r := range batch.Responses {
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
				checkFile(uri, casPresent, blob)
				code, out := httpCall("GetFileRange", map[string]any{"uri": uri, "start": 1, "end": 4})
				if casPresent {
					require.Equal(t, 200, code)
					var r map[string]any
					require.NoError(t, json.Unmarshal(out, &r))
					require.Equal(t, base64.StdEncoding.EncodeToString(blob[1:4]), r["data"])
				} else {
					require.NotEqual(t, 200, code)
					require.Contains(t, string(out), "NotFound")
				}
				metadataCheck(d, false, casPresent)
				metadataCheck(od, false, true)
				if instance == "" {
					cmd := exec.CommandContext(ctx, "bb", "download", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey, fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes))
					out, err := cmd.Output()
					if casPresent {
						require.NoError(t, err)
						require.Equal(t, blob, out)
					} else {
						require.Error(t, err)
					}
				}
				for _, key := range keys {
					got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: key})
					expected, present := model[key.Hash]
					if present {
						require.NoError(t, err)
						require.True(t, proto.Equal(expected, got), "AC result differs: %v", got)
					} else {
						require.Equal(t, codes.NotFound, status.Code(err))
					}
					metadataCheck(key, true, present)
					suffix := fmt.Sprintf("%s/%d", key.Hash, key.SizeBytes)
					rn := harness.ResourceName(instance, key, false)
					acURI := "actioncache://" + f.Endpoint(harness.App).InternalGRPC[len("grpc://"):] + "/" + rn[:len(rn)-len(suffix)] + "ac/" + suffix
					var encoded []byte
					if present {
						encoded, err = proto.Marshal(expected)
						require.NoError(t, err)
					}
					checkFile(acURI, present, encoded)
				}
			}
			update := func(key *repb.Digest, exit int32) {
				result := &repb.ActionResult{ExitCode: exit, ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: "fixture-worker"}, OutputFiles: []*repb.OutputFile{{Path: "out.txt", Digest: d}}}
				got, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: key, ActionResult: result})
				require.NoError(t, err)
				require.True(t, proto.Equal(result, got))
				model[key.Hash] = result
				check()
			}
			deleteFile := func(name string, rest bool) {
				if rest {
					code, b := httpCall("DeleteFile", map[string]any{"uri": name})
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
			// Remove AC references before independently deleting/recreating CAS.
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
				check()
				deleteFile(resource, rest)
				check()
				_, err := c.Write(ctx, instance, blob, false)
				require.NoError(t, err)
				casPresent = true
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
