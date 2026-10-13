package remoteassetstate

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	rapb "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
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
	rn = append(rn, intField(5, uint64(digestFunction(d)))...)
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
	originURL := ""
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--remote_asset.allowed_private_ips=0.0.0.0/0"}, harness.CacheProxy: {"--remote_asset.allowed_private_ips=0.0.0.0/0"}}})
	originURL = fmt.Sprintf("http://%s:%d/asset", fixtureGateway(t, f), port)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	clients := map[harness.Component]*harness.Clients{harness.App: f.Clients(harness.App), harness.CacheProxy: f.Clients(harness.CacheProxy)}
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "second-reader", []string{"CACHE_WRITE"})
	secondClients := map[harness.Component]*harness.Clients{}
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		secondClients[component] = harness.Dial(t, f.Endpoint(component).GRPC, secondKey)
		checkUnsupported(t, ctx, clients[component])
	}
	bb, bbErr := exec.LookPath("bb")
	require.NoError(t, bbErr, "runner must provide bb for CLI read-surface coverage")
	for _, algorithm := range []repb.DigestFunction_Value{repb.DigestFunction_SHA256, repb.DigestFunction_SHA1} {
		wantedDigest := func(data []byte) *repb.Digest {
			if algorithm == repb.DigestFunction_SHA1 {
				hash := sha1.Sum(data)
				return &repb.Digest{Hash: fmt.Sprintf("%x", hash), SizeBytes: int64(len(data))}
			}
			return harness.Digest(data)
		}
		for _, instance := range []string{"", "assets/sequence"} {
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				t.Run(string(component)+"/"+instance+"/"+algorithm.String(), func(t *testing.T) {
					model := map[string][]byte{}
					directories := map[string]*repb.Directory{}
					fetch := func(want []byte, immutable bool, fresh bool) {
						mu.Lock()
						beforeFetch := hits
						mu.Unlock()
						req := &rapb.FetchBlobRequest{InstanceName: instance, Uris: []string{originURL}, DigestFunction: algorithm}
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
						require.True(t, proto.Equal(wantedDigest(want), rsp.GetBlobDigest()), "%v", rsp)
						require.Equal(t, algorithm, rsp.DigestFunction)
						require.Empty(t, rsp.Qualifiers)
						require.Nil(t, rsp.ExpiresAt)
						_, alreadyCached := model[rsp.BlobDigest.Hash]
						mu.Lock()
						afterFetch := hits
						mu.Unlock()
						contacted := afterFetch != beforeFetch
						if algorithm == repb.DigestFunction_SHA256 {
							// Deterministic: immutable repeats must hit CAS, everything else downloads.
							require.Equal(t, !(immutable && alreadyCached), contacted, "checksum hit must reuse CAS; mutable or cold fetch must contact origin")
						} else if !immutable || !alreadyCached {
							require.True(t, contacted, "mutable or cold fetch must contact origin")
						}
						require.LessOrEqual(t, afterFetch, beforeFetch+1)
						// The URI is reported exactly when the origin was contacted. (A sha256 checksum
						// against SHA1 storage may be served through a converted CAS entry.)
						if contacted {
							require.Equal(t, originURL, rsp.Uri)
						} else {
							require.Empty(t, rsp.Uri)
						}
						if component == harness.CacheProxy && len(model) == 0 {
							deadline := time.Now().Add(time.Second)
							for time.Now().Before(deadline) {
								r, e := clients[harness.App].CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{rsp.BlobDigest}})
								require.NoError(t, e)
								require.Len(t, r.MissingBlobDigests, 1)
								time.Sleep(50 * time.Millisecond)
							}
							t.Log("proxy/app population did not converge within 1s; rechecked after every subsequent step")
						}
						model[rsp.BlobDigest.Hash] = append([]byte(nil), want...)
						for _, data := range model {
							d := wantedDigest(data)
							surfaces := []harness.Component{harness.App, harness.CacheProxy}
							if component == harness.CacheProxy {
								surfaces = []harness.Component{harness.CacheProxy}
							}
							for _, surface := range surfaces {
								c := clients[surface]
								_, statusErr := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: harness.UploadResourceName(instance, d, false)})
								require.Equal(t, codes.Unimplemented, status.Code(statusErr))
								if dir := directories[d.Hash]; dir != nil {
									checkDirectory(t, ctx, f, c, surface, instance, d, dir, bb, component == harness.CacheProxy)
								}
								presenceCtx := ctx
								if component == harness.CacheProxy {
									remotePresence, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
									require.NoError(t, err)
									require.Len(t, remotePresence.MissingBlobDigests, 1, "default proxy FindMissingBlobs reflects upstream")
									presenceCtx = metadata.AppendToOutgoingContext(ctx, "proxy_skip_remote", "true")
								}
								started := time.Now()
								require.Eventually(t, func() bool {
									r, e := c.CAS.FindMissingBlobs(presenceCtx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
									return e == nil && len(r.MissingBlobDigests) == 0
								}, 10*time.Second, 50*time.Millisecond)
								t.Logf("%s presence convergence: %s", surface, time.Since(started))
								for stable := 0; stable < 3; stable++ {
									r, e := c.CAS.FindMissingBlobs(presenceCtx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
									require.NoError(t, e)
									require.Empty(t, r.MissingBlobDigests)
									time.Sleep(50 * time.Millisecond)
								}
								other, e := secondClients[surface].Read(ctx, instance, d)
								require.NoError(t, e)
								require.Equal(t, data, other)
								compressed, e := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, true)})
								require.NoError(t, e)
								var packed []byte
								for {
									r, e := compressed.Recv()
									if e == io.EOF {
										break
									}
									require.NoError(t, e)
									packed = append(packed, r.Data...)
								}
								unpacked, e := harness.Decompress(packed)
								require.NoError(t, e)
								require.Equal(t, data, unpacked)
								got, err := c.Read(ctx, instance, d)
								require.NoError(t, err)
								require.Equal(t, data, got)
								batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
								require.NoError(t, err)
								require.Len(t, batch.Responses, 1)
								require.EqualValues(t, 0, batch.Responses[0].Status.Code)
								require.True(t, proto.Equal(d, batch.Responses[0].Digest))
								zipped, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
								require.NoError(t, e)
								require.Len(t, zipped.Responses, 1)
								z := zipped.Responses[0]
								require.EqualValues(t, 0, z.GetStatus().GetCode())
								require.True(t, proto.Equal(d, z.Digest))
								zdata := z.Data
								if z.Compressor == repb.Compressor_ZSTD {
									zdata, e = harness.Decompress(zdata)
									require.NoError(t, e)
								} else {
									require.Equal(t, repb.Compressor_IDENTITY, z.Compressor)
								}
								require.Equal(t, data, zdata)
								require.Equal(t, data, batch.Responses[0].Data)
								md := unary(t, ctx, c, "/cache.service.Cache/GetMetadata", metadataRequest(d, instance))
								checkMetadataFields(t, md)
								_, size := field(md, 5)
								require.EqualValues(t, len(data), size)
								{
									cmd := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(surface).GRPC, "--api_key="+f.APIKey, "/"+harness.ResourceName(instance, d, false))
									got, err := cmd.Output()
									require.NoError(t, err)
									require.Equal(t, data, got)
								}
							}
							if component == harness.App {
								checkApp(t, ctx, f, clients[harness.App], instance, d, data)
							} else {
								// Released proxy fetches deliberately populate only its local CAS.
								missing, err := clients[harness.App].CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
								require.NoError(t, err)
								require.Len(t, missing.MissingBlobDigests, 1)
								checkProxyFileReaders(t, ctx, f, clients[harness.App], instance, d, data)
								auditProxyRange(t, ctx, f, clients[harness.App], instance, d, data)
								checkMissingMetadata(t, ctx, f, clients[harness.App], instance, d)
							}
						}
					}
					mu.Lock()
					content = []byte("asset initial " + algorithm.String() + instance + string(component) + "\n")
					first := append([]byte(nil), content...)
					mu.Unlock()
					fetch(first, true, false)
					mu.Lock()
					before := hits
					mu.Unlock()
					fetch(first, true, false)
					mu.Lock()
					if algorithm == repb.DigestFunction_SHA256 {
						require.Equal(t, before, hits)
					}
					content = []byte("asset updated " + algorithm.String() + instance + string(component) + "\n")
					second := append([]byte(nil), content...)
					mu.Unlock()
					// An immutable checksum still names old content after the origin changes.
					fetch(first, true, false)
					fetch(second, false, true)
					fetch(second, false, true)
					fetch(second, true, false)
					fetch(second, false, false)
					dir := &repb.Directory{Files: []*repb.FileNode{{Name: "retained.txt", Digest: wantedDigest(second)}}}
					encoded, digest, err := harness.MarshalDigest(dir)
					require.NoError(t, err)
					digest = wantedDigest(encoded)
					directories[digest.Hash] = dir
					mu.Lock()
					content = encoded
					mu.Unlock()
					fetch(encoded, false, true)
					mu.Lock()
					content = first
					mu.Unlock()
					fetch(first, false, true)
				})
			}
		}
	}

}

func checkApp(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, instance string, d *repb.Digest, want []byte) {
	t.Helper()
	md := unary(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", metadataRequest(d, instance))
	checkMetadataFields(t, md)
	_, size := field(md, 5)
	require.EqualValues(t, len(want), size)
	web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "instanceName": instance, "cacheType": 2, "digestFunction": int(digestFunction(d))}})
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(len(want)), fmt.Sprint(web["digestSizeBytes"]))
	for _, item := range []struct {
		n   protowire.Number
		key string
	}{{2, "storedSizeBytes"}, {4, "lastModifyUsec"}} {
		_, expected := field(md, item.n)
		require.Equal(t, strconv.FormatUint(expected, 10), fmt.Sprint(web[item.key]))
	}
	access, err := strconv.ParseUint(fmt.Sprint(web["lastAccessUsec"]), 10, 64)
	require.NoError(t, err)
	require.Positive(t, access)
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
		r, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+path, nil)
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
		r, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/"+method, bytes.NewReader(b))
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

// Read only the daemon that owns this fixture, including the harness fallback.
func fixtureGateway(t *testing.T, f *harness.Fixture) string {
	t.Helper()
	sockets, _ := filepath.Glob(filepath.Join(os.TempDir(), "bbval-docker-*", "docker.sock"))
	candidates := [][]string{nil}
	for _, socket := range sockets {
		candidates = append(candidates, []string{"--host", "unix://" + socket})
	}
	for _, prefix := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		args := append(append([]string{}, prefix...), "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}", f.Endpoint(harness.App).Container)
		b, err := exec.CommandContext(ctx, "docker", args...).Output()
		cancel()
		gateway := strings.TrimSpace(string(b))
		if err == nil && net.ParseIP(gateway) != nil {
			return gateway
		}
	}
	t.Fatal("cannot read fixture bridge gateway")
	return ""
}

// The app's full-file readers can address a proxy URI even though its metadata
// and GetFileRange APIs read only the app cache. This exercises that distinction.
func checkProxyFileReaders(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, instance string, d *repb.Digest, want []byte) {
	t.Helper()
	uri := "bytestream://cacheproxy:1985/" + harness.ResourceName(instance, d, false)
	req := bytesField(1, []byte(uri))
	stream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
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
	for _, path := range []string{"/file/download?bytestream_url=" + url.QueryEscape(uri), "/api/v1/GetFile"} {
		method := "GET"
		var body io.Reader
		if path == "/api/v1/GetFile" {
			method = "POST"
			b, _ := json.Marshal(map[string]any{"uri": uri})
			body = bytes.NewReader(b)
		}
		r, err := http.NewRequestWithContext(ctx, method, f.Web.URL+path, body)
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("x-buildbuddy-api-key", f.APIKey)
		res, err := f.Web.Client.Do(r)
		require.NoError(t, err)
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode, "%s", b)
		require.Equal(t, want, b)
	}
}

func auditProxyRange(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, instance string, d *repb.Digest, want []byte) {
	uri := "bytestream://cacheproxy:1985/" + harness.ResourceName(instance, d, false)
	req := bytesField(1, []byte(uri))
	req = append(req, intField(2, 1)...)
	req = append(req, intField(3, uint64(len(want)-1))...)
	var out []byte
	err := c.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", &req, &out, grpc.ForceCodec(wireCodec{}))
	t.Logf("AUDIT same proxy URI: full GetFile succeeded; gRPC GetFileRange error=%v", err)
	require.Error(t, err)
	require.Contains(t, err.Error(), "NotFound")
	b, err := json.Marshal(map[string]any{"uri": uri, "start": "1", "end": strconv.Itoa(len(want) - 1)})
	require.NoError(t, err)
	r, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/GetFileRange", bytes.NewReader(b))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, err := f.Web.Client.Do(r)
	require.NoError(t, err)
	b, err = io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(t, err)
	t.Logf("AUDIT same proxy URI: HTTP GetFileRange status=%d body=%s", res.StatusCode, b)
	require.Equal(t, 500, res.StatusCode)
	require.Contains(t, string(b), "NotFound")
}

func checkMetadataFields(t *testing.T, md []byte) {
	t.Helper()
	_, stored := field(md, 2)
	require.Positive(t, stored)
	for _, n := range []protowire.Number{3, 4} {
		_, stamp := field(md, n)
		require.Positive(t, stamp)
		require.LessOrEqual(t, stamp, uint64(time.Now().Add(time.Minute).UnixMicro()))
	}
}
func checkMissingMetadata(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, instance string, d *repb.Digest) {
	req := metadataRequest(d, instance)
	for _, method := range []string{"/cache.service.Cache/GetMetadata", "/buildbuddy.service.BuildBuddyService/GetCacheMetadata"} {
		var out []byte
		err := c.Conn.Invoke(ctx, method, &req, &out, grpc.ForceCodec(wireCodec{}))
		require.Equal(t, codes.NotFound, status.Code(err))
	}
	_, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "instanceName": instance, "cacheType": 2, "digestFunction": int(digestFunction(d))}})
	require.Error(t, err)
}
func checkUnsupported(t *testing.T, ctx context.Context, c *harness.Clients) {
	_, err := rapb.NewFetchClient(c.Conn).FetchDirectory(ctx, &rapb.FetchDirectoryRequest{Uris: []string{"https://example.invalid/tree"}, DigestFunction: repb.DigestFunction_SHA256})
	require.Equal(t, codes.Unimplemented, status.Code(err))
	_, err = rapb.NewPushClient(c.Conn).PushBlob(ctx, &rapb.PushBlobRequest{Uris: []string{"https://example.invalid/blob"}, BlobDigest: harness.Digest([]byte("valid"))})
	require.Equal(t, codes.Unimplemented, status.Code(err))
	b, d, err := harness.MarshalDigest(&repb.Directory{})
	require.NoError(t, err)
	_ = b
	_, err = rapb.NewPushClient(c.Conn).PushDirectory(ctx, &rapb.PushDirectoryRequest{Uris: []string{"https://example.invalid/tree"}, RootDirectoryDigest: d})
	require.Equal(t, codes.Unimplemented, status.Code(err))
}

func checkDirectory(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, surface harness.Component, instance string, d *repb.Digest, dir *repb.Directory, bb string, proxyOnly bool) {
	stream, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: d, DigestFunction: digestFunction(d)})
	require.NoError(t, err)
	rsp, err := stream.Recv()
	if proxyOnly {
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		t.Logf("proxy-local Directory GetTree divergence: %v", err)
	} else {
		require.NoError(t, err)
		require.Len(t, rsp.Directories, 1)
		require.True(t, proto.Equal(dir, rsp.Directories[0]))
		_, err = stream.Recv()
		require.Equal(t, io.EOF, err)
	}
	cmd := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(surface).GRPC, "--api_key="+f.APIKey, "--type=Directory", "/"+harness.ResourceName(instance, d, false))
	out, err := cmd.Output()
	require.NoError(t, err)
	var decoded repb.Directory
	require.NoError(t, protojson.Unmarshal(out, &decoded))
	require.True(t, proto.Equal(dir, &decoded))
	if proxyOnly {
		checkHTTPAppTree(t, ctx, f, instance, d, nil)
		return
	}
	if surface != harness.App {
		return
	}
	checkHTTPAppTree(t, ctx, f, instance, d, dir)
	digestBytes, err := proto.Marshal(d)
	require.NoError(t, err)
	req := append(bytesField(2, []byte(instance)), bytesField(3, digestBytes)...)
	req = append(req, intField(6, uint64(digestFunction(d)))...)
	ui, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	require.NoError(t, ui.SendMsg(&req))
	require.NoError(t, ui.CloseSend())
	var msg []byte
	require.NoError(t, ui.RecvMsg(&msg))
	raw, _ := field(msg, 2)
	require.NoError(t, proto.Unmarshal(raw, &decoded))
	require.True(t, proto.Equal(dir, &decoded))
	sizes, err := f.Web.RPC("GetTreeDirectorySizes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "rootDigest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "instanceName": instance, "digestFunction": int(digestFunction(d))})
	require.NoError(t, err)
	entries, ok := sizes["sizes"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 1)
	entry := entries[0].(map[string]any)
	require.Equal(t, d.Hash, entry["digest"])
	require.Equal(t, strconv.FormatInt(d.SizeBytes+dir.Files[0].Digest.SizeBytes, 10), fmt.Sprint(entry["totalSize"]))
	require.Equal(t, "1", fmt.Sprint(entry["childCount"]))
}

// Matches app/service/rpc_service.ts streaming proto+prefixed transport.
func checkHTTPAppTree(t *testing.T, ctx context.Context, f *harness.Fixture, instance string, d *repb.Digest, want *repb.Directory) {
	digestBytes, err := proto.Marshal(d)
	require.NoError(t, err)
	req := append(bytesField(2, []byte(instance)), bytesField(3, digestBytes)...)
	req = append(req, intField(6, uint64(digestFunction(d)))...)
	body := make([]byte, 5)
	binary.BigEndian.PutUint32(body[1:], uint32(len(req)))
	body = append(body, req...)
	r, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/GetTree", bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/proto+prefixed")
	r.Header.Set("x-buildbuddy-api-key", f.APIKey)
	rsp, err := f.Web.Client.Do(r)
	require.NoError(t, err)
	out, err := io.ReadAll(rsp.Body)
	rsp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, rsp.StatusCode)
	if want == nil {
		if rsp.Header.Get("grpc-status") != "" {
			require.Equal(t, "9", rsp.Header.Get("grpc-status"))
		} else {
			require.GreaterOrEqual(t, len(out), 5)
			require.EqualValues(t, 0x80, out[0])
			require.Contains(t, string(out[5:]), "Grpc-Status: 9")
		}
		return
	}
	require.GreaterOrEqual(t, len(out), 5)
	require.EqualValues(t, 0, out[0])
	size := int(binary.BigEndian.Uint32(out[1:5]))
	require.GreaterOrEqual(t, len(out), 5+size)
	raw, _ := field(out[5:5+size], 2)
	var dir repb.Directory
	require.NoError(t, proto.Unmarshal(raw, &dir))
	require.True(t, proto.Equal(want, &dir))
	require.Contains(t, string(out[5+size:]), "Grpc-Status: 0")
}

func digestFunction(d *repb.Digest) repb.DigestFunction_Value {
	if len(d.Hash) == 40 {
		return repb.DigestFunction_SHA1
	}
	return repb.DigestFunction_SHA256
}
