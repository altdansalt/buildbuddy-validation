package metadata_test

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
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
							t.Skip("runner has no bb CLI; requires a pinned CLI runfile")
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
	for endpoint, c := range clients {
		t.Run(fmt.Sprintf("AC/%d", endpoint), func(t *testing.T) {
			d := harness.Digest([]byte(fmt.Sprintf("action-key-%d", endpoint)))
			for _, reader := range clients {
				_, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d})
				require.Equal(t, codes.NotFound, status.Code(err))
			}
			for _, stdout := range []string{"first", "a longer replacement result", "a longer replacement result"} {
				model := &repb.ActionResult{StdoutRaw: []byte(stdout)}
				_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, ActionResult: model})
				require.NoError(t, err)
				for _, reader := range clients {
					require.EventuallyWithT(t, func(ct *assert.CollectT) {
						got, err := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, InlineStdout: true})
						require.NoError(ct, err)
						if err == nil {
							require.Equal(ct, model.StdoutRaw, got.StdoutRaw)
						}
					}, 10*time.Second, 100*time.Millisecond)
				}
				checkMetadata(d, 1)
			}
		})
	}
}
