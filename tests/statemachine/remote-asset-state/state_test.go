package remoteassetstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	rapb "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The repository has no generated BuildBuddy API bindings. These wire messages
// use the released proto/cache.proto, resource.proto and api/v1/file.proto field
// numbers; ForceCodec is call-local and retains the ordinary protobuf wire type.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func bytesField(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func intField(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func field(b []byte, n protowire.Number) ([]byte, uint64) {
	for len(b) > 0 {
		tag, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			return nil, 0
		}
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				return nil, 0
			}
			if tag == n {
				return v, 0
			}
			b = b[k:]
		} else if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				return nil, 0
			}
			if tag == n {
				return nil, v
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(tag, typ, b)
			if k < 0 {
				return nil, 0
			}
			b = b[k:]
		}
	}
	return nil, 0
}
func unary(t *testing.T, ctx context.Context, c *harness.Clients, method string, req []byte) []byte {
	t.Helper()
	var out []byte
	require.NoError(t, c.Conn.Invoke(ctx, method, &req, &out, grpc.ForceCodec(wireCodec{})))
	return out
}
func metadataRequest(d *repb.Digest, instance string) []byte {
	b, _ := proto.Marshal(d)
	rn := bytesField(1, b)
	rn = append(rn, bytesField(2, []byte(instance))...)
	rn = append(rn, intField(4, 2)...)
	rn = append(rn, intField(5, 1)...)
	return bytesField(2, rn)
}

func TestFetchBlobStateMachine(t *testing.T) {
	var mu sync.Mutex
	content := []byte("asset version one\n")
	hits := 0
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	origin := &http.Server{ReadHeaderTimeout: time.Second * 5, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(content)
	})}
	go origin.Serve(listener)
	t.Cleanup(func() { origin.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	originURL := fmt.Sprintf("http://host.docker.internal:%d/asset", port)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--remote_asset.allowed_private_ips=0.0.0.0/0"}, harness.CacheProxy: {"--remote_asset.allowed_private_ips=0.0.0.0/0"}}, DockerArgs: map[harness.Component][]string{harness.App: {"--add-host=host.docker.internal:host-gateway"}, harness.CacheProxy: {"--add-host=host.docker.internal:host-gateway"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	clients := map[harness.Component]*harness.Clients{harness.App: f.Clients(harness.App), harness.CacheProxy: f.Clients(harness.CacheProxy)}
	bb, bbErr := exec.LookPath("bb")
	if bbErr != nil {
		t.Log("bb CLI unavailable on runner; ByteStream backing surface remains checked")
	}
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			instance := "remote-asset-state/" + string(component)
			model := map[string][]byte{}
			fetch := func(want []byte, immutable bool, fresh bool) {
				mu.Lock()
				beforeFetch := hits
				mu.Unlock()
				req := &rapb.FetchBlobRequest{InstanceName: instance, Uris: []string{originURL}, DigestFunction: repb.DigestFunction_SHA256}
				if immutable {
					h := sha256.Sum256(want)
					req.Qualifiers = []*rapb.Qualifier{{Name: "checksum.sri", Value: "sha256-" + base64.StdEncoding.EncodeToString(h[:])}}
				}
				if fresh {
					req.OldestContentAccepted = timestamppb.Now()
				}
				rsp, err := rapb.NewFetchClient(clients[component].Conn).FetchBlob(ctx, req)
				require.NoError(t, err)
				require.EqualValues(t, 0, rsp.GetStatus().GetCode(), "%v", rsp)
				require.True(t, proto.Equal(harness.Digest(want), rsp.GetBlobDigest()), "%v", rsp)
				_, alreadyCached := model[rsp.BlobDigest.Hash]
				mu.Lock()
				afterFetch := hits
				mu.Unlock()
				if immutable && alreadyCached {
					require.Equal(t, beforeFetch, afterFetch, "checksum hit must reuse CAS")
				} else {
					require.Equal(t, beforeFetch+1, afterFetch, "mutable or cold fetch must contact origin")
				}
				model[rsp.BlobDigest.Hash] = append([]byte(nil), want...)
				for _, data := range model {
					d := harness.Digest(data)
					for _, surface := range []harness.Component{harness.App, harness.CacheProxy} {
						c := clients[surface]
						require.Eventually(t, func() bool {
							r, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
							return e == nil && len(r.MissingBlobDigests) == 0
						}, 10*time.Second, 50*time.Millisecond)
						got, err := c.Read(ctx, instance, d)
						require.NoError(t, err)
						require.Equal(t, data, got)
						batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
						require.NoError(t, err)
						require.Len(t, batch.Responses, 1)
						require.EqualValues(t, 0, batch.Responses[0].Status.Code)
						require.Equal(t, data, batch.Responses[0].Data)
						md := unary(t, ctx, c, "/cache.service.Cache/GetMetadata", metadataRequest(d, instance))
						_, size := field(md, 5)
						require.EqualValues(t, len(data), size)
						if bbErr == nil {
							cmd := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(surface).GRPC, "--api_key="+f.APIKey, "/"+harness.ResourceName(instance, d, false))
							got, err := cmd.Output()
							require.NoError(t, err)
							require.Equal(t, data, got)
						}
					}
					checkApp(t, ctx, f, clients[harness.App], instance, d, data)
				}
			}
			mu.Lock()
			content = []byte("asset initial " + string(component) + "\n")
			first := append([]byte(nil), content...)
			mu.Unlock()
			fetch(first, true, false)
			mu.Lock()
			before := hits
			mu.Unlock()
			fetch(first, true, false)
			mu.Lock()
			require.Equal(t, before, hits)
			content = []byte("asset updated " + string(component) + "\n")
			second := append([]byte(nil), content...)
			mu.Unlock()
			// An immutable checksum still names old content after the origin changes.
			fetch(first, true, false)
			fetch(second, false, true)
			fetch(second, false, true)
			fetch(second, true, false)
			fetch(second, false, false)
		})
	}
}

func checkApp(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, instance string, d *repb.Digest, want []byte) {
	t.Helper()
	md := unary(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", metadataRequest(d, instance))
	_, size := field(md, 5)
	require.EqualValues(t, len(want), size)
	web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "instanceName": instance, "cacheType": 2, "digestFunction": 1}})
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(len(want)), fmt.Sprint(web["digestSizeBytes"]))
	uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, false)
	req := bytesField(1, []byte(uri))
	req = append(req, intField(2, 1)...)
	req = append(req, intField(3, uint64(len(want)-1))...)
	result := unary(t, ctx, c, "/api.v1.ApiService/GetFileRange", req)
	got, _ := field(result, 1)
	require.Equal(t, want[1:len(want)-1], got)
	stream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	req = bytesField(1, []byte(uri))
	require.NoError(t, stream.SendMsg(&req))
	require.NoError(t, stream.CloseSend())
	var all []byte
	for {
		var msg []byte
		err := stream.RecvMsg(&msg)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		b, _ := field(msg, 1)
		all = append(all, b...)
	}
	require.Equal(t, want, all)
	for _, path := range []string{"/file/download?bytestream_url=" + url.QueryEscape(uri)} {
		r, err := http.NewRequestWithContext(ctx, "GET", f.Endpoint(harness.App).HTTP+path, nil)
		require.NoError(t, err)
		r.Header.Set("x-buildbuddy-api-key", f.APIKey)
		res, err := f.Web.Client.Do(r)
		require.NoError(t, err)
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode, "%s", b)
		require.Equal(t, want, b)
	}
	for _, method := range []string{"GetFile", "GetFileRange"} {
		payload := map[string]any{"uri": uri}
		if method == "GetFileRange" {
			payload["start"] = "1"
			payload["end"] = strconv.Itoa(len(want) - 1)
		}
		b, err := json.Marshal(payload)
		require.NoError(t, err)
		r, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(b))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("x-buildbuddy-api-key", f.APIKey)
		res, err := f.Web.Client.Do(r)
		require.NoError(t, err)
		b, err = io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode, "%s", b)
		if method == "GetFile" {
			require.Equal(t, want, b)
		} else {
			var out struct{ Data []byte }
			require.NoError(t, json.Unmarshal(b, &out))
			require.Equal(t, want[1:len(want)-1], out.Data)
		}
	}
}
