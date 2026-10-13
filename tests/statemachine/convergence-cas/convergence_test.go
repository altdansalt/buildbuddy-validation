package convergencecas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type blob struct {
	data    []byte
	digest  *repb.Digest
	present bool
}

// The public API's request URI and streamed response data occupy field 1,
// exactly like StringValue and BytesValue. These wire-compatible messages avoid
// adding BuildBuddy source/generated dependencies to this released-image test.
func getFile(ctx context.Context, c *harness.Clients, uri string) ([]byte, error) {
	s, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
	if err != nil {
		return nil, err
	}
	if err = s.SendMsg(wrapperspb.String(uri)); err != nil {
		return nil, err
	}
	if err = s.CloseSend(); err != nil {
		return nil, err
	}
	var data []byte
	for {
		m := new(wrapperspb.BytesValue)
		err := s.RecvMsg(m)
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
		data = append(data, m.Value...)
	}
}

// Minimal public wire fields: URI=1/start=2/end=3 and resource_name=2.
func getRange(ctx context.Context, c *harness.Clients, uri string, size int64) ([]byte, error) {
	req := wrapperspb.String(uri)
	wire := protowire.AppendTag(nil, 3, protowire.VarintType)
	wire = protowire.AppendVarint(wire, uint64(size))
	req.ProtoReflect().SetUnknown(wire)
	res := new(wrapperspb.BytesValue)
	err := c.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", req, res)
	return res.Value, err
}
func grpcMetadata(ctx context.Context, c *harness.Clients, d *repb.Digest) error {
	digest := protowire.AppendTag(nil, 1, protowire.BytesType)
	digest = protowire.AppendString(digest, d.Hash)
	digest = protowire.AppendTag(digest, 2, protowire.VarintType)
	digest = protowire.AppendVarint(digest, uint64(d.SizeBytes))
	resource := protowire.AppendTag(nil, 1, protowire.BytesType)
	resource = protowire.AppendBytes(resource, digest)
	resource = protowire.AppendTag(resource, 4, protowire.VarintType)
	resource = protowire.AppendVarint(resource, 2)
	req := new(wrapperspb.BytesValue)
	wire := protowire.AppendTag(nil, 2, protowire.BytesType)
	wire = protowire.AppendBytes(wire, resource)
	req.ProtoReflect().SetUnknown(wire)
	return c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", req, new(wrapperspb.BytesValue))
}

func httpRead(ctx context.Context, f *harness.Fixture, path string, body any) ([]byte, int, error) {
	var r io.Reader
	method := "GET"
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, f.Endpoint(harness.App).HTTP+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return b, res.StatusCode, err
}

func cli(t *testing.T, ctx context.Context) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	b, err := io.ReadAll(io.LimitReader(res.Body, 100<<20))
	require.NoError(t, err)
	require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(b)))
	p := filepath.Join(t.TempDir(), "bb")
	require.NoError(t, os.WriteFile(p, b, 0755))
	return p
}

func TestBatchCASConvergence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binary := cli(t, ctx)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	clients := map[harness.Component]*harness.Clients{harness.App: f.Clients(harness.App), harness.CacheProxy: f.Clients(harness.CacheProxy)}
	// Empty CAS is canonical and logically present without any write.
	model := []*blob{{data: []byte{}, digest: harness.Digest(nil), present: true}}
	for _, data := range [][]byte{[]byte("app batch first\x00\xff"), bytes.Repeat([]byte("proxy flat blob\x00"), 8193), []byte("later app addition"), []byte("later proxy addition")} {
		model = append(model, &blob{data: data, digest: harness.Digest(data)})
	}
	check := func(ctx context.Context) error {
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			c := clients[component]
			var ds []*repb.Digest
			for _, b := range model {
				ds = append(ds, b.digest)
			}
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: ds, DigestFunction: repb.DigestFunction_SHA256})
			if err != nil {
				return err
			}
			absent := map[string]bool{}
			for _, d := range missing.MissingBlobDigests {
				if absent[d.Hash] {
					return fmt.Errorf("duplicate missing digest")
				}
				absent[d.Hash] = true
			}
			reads, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: ds, DigestFunction: repb.DigestFunction_SHA256})
			if err != nil {
				return err
			}
			got := map[string]*repb.BatchReadBlobsResponse_Response{}
			for _, r := range reads.Responses {
				if got[r.GetDigest().GetHash()] != nil {
					return fmt.Errorf("duplicate batch response")
				}
				got[r.GetDigest().GetHash()] = r
			}
			if len(got) != len(model) {
				return fmt.Errorf("%s batch response count %d", component, len(got))
			}
			for _, b := range model {
				r := got[b.digest.Hash]
				if r == nil || absent[b.digest.Hash] == b.present {
					return fmt.Errorf("%s presence mismatch %s", component, b.digest.Hash)
				}
				data, err := c.Read(ctx, "", b.digest)
				if !b.present {
					if r.GetStatus().GetCode() != int32(codes.NotFound) || status.Code(err) != codes.NotFound {
						return fmt.Errorf("%s absent blob read: batch=%v stream=%v", component, r, err)
					}
					continue
				}
				if r.GetStatus().GetCode() != 0 || r.GetCompressor() != repb.Compressor_IDENTITY || !bytes.Equal(r.Data, b.data) || err != nil || !bytes.Equal(data, b.data) {
					return fmt.Errorf("%s bytes mismatch %s: %v", component, b.digest.Hash, err)
				}
			}
		}
		return nil
	}
	converge := func() {
		t.Helper()
		pollCtx, pollCancel := context.WithTimeout(ctx, 20*time.Second)
		defer pollCancel()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			err := check(pollCtx)
			if err == nil {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-pollCtx.Done():
				t.Fatalf("convergence deadline: %v", err)
			case <-tick.C:
			}
		}
	}
	surfaces := func() {
		t.Helper()
		for _, b := range model {
			if !b.present {
				uri := "bytestream://app:1985/" + harness.ResourceName("", b.digest, false)
				missingData, err := getFile(ctx, clients[harness.App], uri)
				require.Empty(t, missingData)
				require.Equal(t, codes.Unavailable, status.Code(err), "pooled reader sanitizes missing errors")
				_, err = getRange(ctx, clients[harness.App], uri, b.digest.SizeBytes)
				require.Equal(t, codes.NotFound, status.Code(err))
				require.Equal(t, codes.NotFound, status.Code(grpcMetadata(ctx, clients[harness.App], b.digest)))
				metadataData, metadataCode, metadataErr := httpRead(ctx, f, "/rpc/BuildBuddyService/GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": b.digest.Hash, "sizeBytes": b.digest.SizeBytes}, "cacheType": 2, "digestFunction": 1}})
				require.NoError(t, metadataErr)
				require.NotEqual(t, 200, metadataCode)
				require.Contains(t, string(metadataData), "NotFound")
				for _, endpoint := range []string{"/api/v1/GetFile", "/api/v1/GetFileRange"} {
					body := map[string]any{"uri": uri}
					if endpoint == "/api/v1/GetFileRange" {
						body["end"] = b.digest.SizeBytes
					}
					data, code, err := httpRead(ctx, f, endpoint, body)
					require.NoError(t, err)
					require.NotEqual(t, 200, code, string(data))
					if endpoint == "/api/v1/GetFileRange" {
						require.Contains(t, string(data), "NotFound")
					} else {
						require.Equal(t, 404, code)
					}
				}
				for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
					cmd := exec.CommandContext(ctx, binary, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, fmt.Sprintf("%s/%d", b.digest.Hash, b.digest.SizeBytes))
					for _, e := range os.Environ() {
						if !strings.HasPrefix(e, "BUILDBUDDY_API_KEY=") {
							cmd.Env = append(cmd.Env, e)
						}
					}
					log, err := cmd.CombinedOutput()
					require.Error(t, err)
					require.Contains(t, string(log), "NotFound")
				}
				continue
			}
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				// URI host must resolve inside the app container, never a worker loopback.
				uri := "bytestream://" + strings.TrimPrefix(f.Endpoint(component).InternalGRPC, "grpc://") + "/" + harness.ResourceName("", b.digest, false)
				data, err := getFile(ctx, clients[harness.App], uri)
				require.NoError(t, err)
				require.True(t, bytes.Equal(b.data, data), "exact bytes")
				restData, restCode, restErr := httpRead(ctx, f, "/api/v1/GetFile", map[string]any{"uri": uri})
				require.NoError(t, restErr)
				require.Equal(t, 200, restCode, string(restData))
				require.True(t, bytes.Equal(b.data, restData))
				for _, path := range []string{"/file/download?bytestream_url=" + url.QueryEscape(uri), "/file/view?bytestream_url=" + url.QueryEscape(uri)} {
					data, code, err := httpRead(ctx, f, path, nil)
					require.NoError(t, err)
					require.Equal(t, 200, code, string(data))
					require.Equal(t, b.data, data, path)
				}
				out := filepath.Join(t.TempDir(), "blob")
				cmd := exec.CommandContext(ctx, binary, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+out, fmt.Sprintf("%s/%d", b.digest.Hash, b.digest.SizeBytes))
				// Never inherit the dispatch credential into the CLI client.
				for _, e := range os.Environ() {
					if !strings.HasPrefix(e, "BUILDBUDDY_API_KEY=") {
						cmd.Env = append(cmd.Env, e)
					}
				}
				log, err := cmd.CombinedOutput()
				require.NoError(t, err, string(log))
				data, err = os.ReadFile(out)
				require.NoError(t, err)
				require.True(t, bytes.Equal(b.data, data), "exact bytes")
			}
			if len(b.data) > 0 {
				uri := "bytestream://app:1985/" + harness.ResourceName("", b.digest, false)
				rangeData, rangeErr := getRange(ctx, clients[harness.App], uri, b.digest.SizeBytes)
				require.NoError(t, rangeErr)
				require.True(t, bytes.Equal(b.data, rangeData))
				require.NoError(t, grpcMetadata(ctx, clients[harness.App], b.digest))
				data, code, err := httpRead(ctx, f, "/api/v1/GetFileRange", map[string]any{"uri": uri, "start": 0, "end": b.digest.SizeBytes})
				require.NoError(t, err)
				require.Equal(t, 200, code, string(data))
				var response struct{ Data string }
				require.NoError(t, json.Unmarshal(data, &response))
				decoded, err := base64.StdEncoding.DecodeString(response.Data)
				require.NoError(t, err)
				require.Equal(t, b.data, decoded)
			}
			if len(b.data) == 0 {
				continue
			} // Empty CAS has virtual presence, no physical metadata.
			data, code, err := httpRead(ctx, f, "/rpc/BuildBuddyService/GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": b.digest.Hash, "sizeBytes": b.digest.SizeBytes}, "cacheType": 2, "digestFunction": 1}})
			require.NoError(t, err)
			require.Equal(t, 200, code, string(data))
		}
	}
	converge()
	surfaces()
	// Mix new and previously accepted blobs, then repeat each identical batch.
	for step, component := range []harness.Component{harness.App, harness.CacheProxy, harness.App, harness.CacheProxy} {
		batch := []*blob{model[step+1], model[0]}
		if step > 0 {
			batch = append(batch, model[step])
		}
		for repeat := 0; repeat < 2; repeat++ {
			var requests []*repb.BatchUpdateBlobsRequest_Request
			for _, b := range batch {
				requests = append(requests, &repb.BatchUpdateBlobsRequest_Request{Digest: b.digest, Data: b.data})
			}
			res, err := clients[component].CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: requests, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, res.Responses, len(batch))
			accepted := map[string]bool{}
			for _, r := range res.Responses {
				require.EqualValues(t, 0, r.GetStatus().GetCode())
				accepted[r.GetDigest().GetHash()] = true
			}
			for _, b := range batch {
				require.True(t, accepted[b.digest.Hash])
				b.present = true
			}
			t.Logf("step=%d writer=%s repeat=%d", step, component, repeat)
			converge()
			surfaces()
		}
	}
}
