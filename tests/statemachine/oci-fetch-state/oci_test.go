package ocistate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const repo = "gcr.io/flame-public/buildbuddy-app-enterprise"
const manifestHash = "ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843"
const instance = "_bb_ociregistry_"

// Wire codec keeps the test independent of BuildBuddy's source build. Field
// numbers are from released proto/oci_fetcher.proto and proto/ociregistry.proto.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func field(b []byte, n protowire.Number, s string) []byte {
	b = protowire.AppendTag(b, n, protowire.BytesType)
	return protowire.AppendString(b, s)
}
func number(b []byte, n protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, n, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}
func fields(t *testing.T, b []byte) (map[protowire.Number][]byte, map[protowire.Number]uint64) {
	t.Helper()
	s := map[protowire.Number][]byte{}
	v := map[protowire.Number]uint64{}
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		b = b[k:]
		switch typ {
		case protowire.BytesType:
			x, k := protowire.ConsumeBytes(b)
			require.Greater(t, k, 0)
			s[n] = x
			b = b[k:]
		case protowire.VarintType:
			x, k := protowire.ConsumeVarint(b)
			require.Greater(t, k, 0)
			v[n] = x
			b = b[k:]
		default:
			t.Fatalf("unexpected wire type %v", typ)
		}
	}
	return s, v
}
func call(t *testing.T, ctx context.Context, c *harness.Clients, method string, req []byte) []byte {
	t.Helper()
	var resp []byte
	require.NoError(t, c.Conn.Invoke(ctx, method, &req, &resp, grpc.ForceCodec(wireCodec{})))
	return resp
}
func fetch(t *testing.T, ctx context.Context, c *harness.Clients, method, ref string) []byte {
	return call(t, ctx, c, "/oci_fetcher.OCIFetcher/"+method, field(nil, 1, ref))
}
func blob(t *testing.T, ctx context.Context, c *harness.Clients, ref string) []byte {
	t.Helper()
	s, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/oci_fetcher.OCIFetcher/FetchBlob", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	req := field(nil, 1, ref)
	require.NoError(t, s.SendMsg(&req))
	require.NoError(t, s.CloseSend())
	var out []byte
	for {
		var b []byte
		err := s.RecvMsg(&b)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		f, _ := fields(t, b)
		out = append(out, f[1]...)
	}
	return out
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func manifestKey() *repb.Digest {
	var s string
	for _, x := range []string{"gcr.io", "flame-public/buildbuddy-app-enterprise", "MANIFEST", "sha256", manifestHash, ""} {
		s += hash([]byte(x))
	}
	return harness.Digest([]byte(hash([]byte(s))))
}
func blobKey(h string) *repb.Digest {
	b := field(nil, 1, "gcr.io")
	b = field(b, 2, "flame-public/buildbuddy-app-enterprise")
	b = number(b, 3, 1)
	b = field(b, 4, "sha256")
	b = field(b, 5, h)
	return harness.Digest(b)
}
func action(ctx context.Context, c *harness.Clients, inst string, d *repb.Digest) (*repb.ActionResult, error) {
	return c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: inst, ActionDigest: d})
}

func cliBinary(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.466/bazel-5.0.466-linux-x86_64", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	require.NoError(t, err)
	require.Equal(t, "df61a209cc45ecff9059bb04e01cc1dbc2b98a128715cada2a2a54bb4deba485", hash(b))
	path := filepath.Join(t.TempDir(), "bb")
	require.NoError(t, os.WriteFile(path, b, 0700))
	return path
}

// Check the same resource through the app gRPC API and its cookie-authenticated
// JSON endpoint. Stored size can differ from digest size due to compression.
func uiMetadata(t *testing.T, ctx context.Context, f *harness.Fixture, app *harness.Clients, inst string, d *repb.Digest, cacheType string) {
	t.Helper()
	ct := uint64(2)
	if cacheType == "AC" {
		ct = 1
	}
	resource := field(nil, 1, string(number(field(nil, 1, d.Hash), 2, uint64(d.SizeBytes))))
	resource = field(resource, 2, inst)
	resource = number(resource, 4, ct)
	resource = number(resource, 5, 1)
	_, v := fields(t, call(t, ctx, app, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", field(nil, 2, string(resource))))
	require.Equal(t, uint64(d.SizeBytes), v[5])
	require.Positive(t, v[2])
	require.Positive(t, v[3])
	require.Positive(t, v[4])
	ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "instanceName": inst, "cacheType": cacheType, "digestFunction": "SHA256"}})
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(d.SizeBytes, 10), fmt.Sprint(ui["digestSizeBytes"]))
	for _, name := range []string{"storedSizeBytes", "lastAccessUsec", "lastModifyUsec"} {
		value, err := strconv.ParseInt(fmt.Sprint(ui[name]), 10, 64)
		require.NoError(t, err)
		require.Positive(t, value, name)
	}
	require.Equal(t, strconv.FormatUint(v[2], 10), fmt.Sprint(ui["storedSizeBytes"]))
	require.Equal(t, strconv.FormatUint(v[4], 10), fmt.Sprint(ui["lastModifyUsec"]))
}

// Audit probe: exercise the actual frontend file content handlers with cookies,
// selected-group context, and container-local targets. Never fall back upstream.
func httpContent(t *testing.T, ctx context.Context, f *harness.Fixture, inst string, d *repb.Digest, ac bool, expected []byte) {
	t.Helper()
	q := url.Values{}
	scheme, part := "bytestream", "blobs"
	if ac {
		scheme, part = "actioncache", "blobs/ac"
	}
	q.Set("bytestream_url", scheme+"://localhost:1985/"+inst+"/"+part+"/"+d.Hash+"/"+strconv.FormatInt(d.SizeBytes, 10))
	q.Set("request_context", base64.StdEncoding.EncodeToString(field(nil, 2, f.GroupID)))
	for _, endpoint := range []string{"/file/download", "/file/view", "/api/v1/GetFile"} {
		req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+endpoint+"?"+q.Encode(), nil)
		if endpoint == "/api/v1/GetFile" {
			body, marshalErr := json.Marshal(map[string]string{"uri": q.Get("bytestream_url")})
			require.NoError(t, marshalErr)
			req, err = http.NewRequestWithContext(ctx, "POST", f.Web.URL+endpoint, bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		}
		require.NoError(t, err)
		resp, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		require.Equal(t, expected, body)

	}
	stream, err := f.Clients(harness.App).Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	req := field(nil, 1, q.Get("bytestream_url"))
	require.NoError(t, stream.SendMsg(&req))
	require.NoError(t, stream.CloseSend())
	var got []byte
	for {
		var b []byte
		err := stream.RecvMsg(&b)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		s, _ := fields(t, b)
		got = append(got, s[1]...)
	}
	require.Equal(t, expected, got)

}

func TestFetchState(t *testing.T) {
	cli := cliBinary(t)
	for _, proxyFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxy_first_%v", proxyFirst), func(t *testing.T) { testFetchState(t, proxyFirst, cli) })
	}
}

func testFetchState(t *testing.T, proxyFirst bool, cli string) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"ocifetcher": harness.Config{"enabled": true}, "ociregistry": harness.Config{"enabled": true}, "app": harness.Config{"client_identity": harness.Config{"key": "oci-state-fixture-key", "client": "app"}}}, harness.CacheProxy: {"app": harness.Config{"client_identity": harness.Config{"key": "oci-state-fixture-key", "client": "cache-proxy"}}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	// OCI AC records are intentionally restricted to signed internal service identities.
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"exp": time.Now().Add(10 * time.Minute).Unix(), "Origin": "internal", "Client": "executor"}).SignedString([]byte("oci-state-fixture-key"))
	require.NoError(t, err)
	ctx = metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-client-identity", token)
	app := f.Clients(harness.App)
	clients := []*harness.Clients{app, f.Clients(harness.CacheProxy)}
	key2 := f.Web.CreateAPIKey(t, f.GroupID, "oci-state-second", []string{"CACHE_WRITE", "CAS_WRITE"})
	secondApp := harness.Dial(t, app.Conn.Target(), key2)
	secondProxy := harness.Dial(t, clients[1].Conn.Target(), key2)
	if proxyFirst {
		clients[0], clients[1] = clients[1], clients[0]
	}
	clients = append(clients, secondApp, secondProxy)
	ref := repo + "@sha256:" + manifestHash
	registryReq, err := http.NewRequestWithContext(ctx, "GET", "https://gcr.io/v2/flame-public/buildbuddy-app-enterprise/manifests/sha256:"+manifestHash, nil)
	require.NoError(t, err)
	registryReq.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	registryResp, err := http.DefaultClient.Do(registryReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, registryResp.StatusCode)
	registryBytes, err := io.ReadAll(registryResp.Body)
	registryResp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, manifestHash, hash(registryBytes))
	var manifest struct {
		Config struct {
			Digest string
			Size   int64
		}
	}
	require.NoError(t, json.Unmarshal(registryBytes, &manifest))
	require.NotEmpty(t, manifest.Config.Digest)
	h := strings.TrimPrefix(manifest.Config.Digest, "sha256:")
	d := &repb.Digest{Hash: h, SizeBytes: manifest.Config.Size}
	bref := repo + "@" + manifest.Config.Digest
	// Metadata-only requests must leave persistent manifest state absent.
	for _, c := range clients {
		m, v := fields(t, fetch(t, ctx, c, "FetchManifestMetadata", ref))
		require.Equal(t, "sha256:"+manifestHash, string(m[1]))
		require.Equal(t, uint64(len(registryBytes)), v[2])
		require.Equal(t, "application/vnd.docker.distribution.manifest.v2+json", string(m[3]))
		_, err := action(ctx, c, instance+"_manifest_content_", manifestKey())
		require.Equal(t, codes.NotFound, status.Code(err))
		for _, read := range clients {
			absent(t, ctx, f, cli, read, instance+"_manifest_content_", manifestKey(), true)
			absent(t, ctx, f, cli, read, instance, blobKey(h), true)
			absent(t, ctx, f, cli, read, instance, d, false)
		}
	}
	var raw = registryBytes
	var media = "application/vnd.docker.distribution.manifest.v2+json"
	for round := 0; round < 2; round++ {
		for _, c := range clients {
			m, v := fields(t, fetch(t, ctx, c, "FetchManifest", ref))
			require.Equal(t, manifestHash, hash(m[4]))
			require.Equal(t, uint64(len(m[4])), v[2])
			require.Equal(t, "sha256:"+manifestHash, string(m[1]))
			if raw == nil {
				raw = append([]byte(nil), m[4]...)
				media = string(m[3])
			}
			require.Equal(t, raw, m[4])
			require.Equal(t, "application/vnd.docker.distribution.manifest.v2+json", string(m[3]))
			require.Equal(t, media, string(m[3]))
			meta, mv := fields(t, fetch(t, ctx, c, "FetchManifestMetadata", ref))
			require.Equal(t, m[1], meta[1])
			require.Equal(t, m[3], meta[3])
			require.Equal(t, v[2], mv[2])
			manifestSurfaces(t, ctx, f, app, clients, raw, media)
			for _, read := range clients {
				absent(t, ctx, f, cli, read, instance, blobKey(h), true)
				absent(t, ctx, f, cli, read, instance, d, false)
			}
			mirrorContent(t, ctx, f, "manifests", "sha256:"+manifestHash, raw)
		}
	}
	var blobMedia []byte
	for _, c := range clients {
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, missing.MissingBlobDigests, 1)
		_, err = action(ctx, c, instance, blobKey(h))
		require.Equal(t, codes.NotFound, status.Code(err))
		bm, v := fields(t, fetch(t, ctx, c, "FetchBlobMetadata", bref))
		require.Equal(t, "application/vnd.docker.image.rootfs.diff.tar.gzip", string(bm[2]))
		if blobMedia == nil {
			blobMedia = append([]byte(nil), bm[2]...)
		}
		require.Equal(t, blobMedia, bm[2])
		require.Equal(t, uint64(d.SizeBytes), v[1])
		_, err = action(ctx, c, instance, blobKey(h))
		require.Equal(t, codes.NotFound, status.Code(err))
		missing, err = c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, missing.MissingBlobDigests, 1)
		manifestSurfaces(t, ctx, f, app, clients, raw, media)
		for _, read := range clients {
			absent(t, ctx, f, cli, read, instance, blobKey(h), true)
			absent(t, ctx, f, cli, read, instance, d, false)
		}
	}
	var model []byte
	for round := 0; round < 2; round++ {
		for _, c := range clients {
			b := blob(t, ctx, c, bref)
			require.Equal(t, h, hash(b))
			require.Equal(t, d.SizeBytes, int64(len(b)))
			if model == nil {
				model = append([]byte(nil), b...)
			}
			require.Equal(t, model, b)
			// Fetch completion commits synchronously. Poll AC only to bound any proxy propagation.
			for _, read := range clients {
				start := time.Now()
				var ar *repb.ActionResult
				for {
					var err error
					ar, err = action(ctx, read, instance, blobKey(h))
					if err == nil {
						break
					}
					require.Equal(t, codes.NotFound, status.Code(err))
					require.Less(t, time.Since(start), 10*time.Second, "AC convergence deadline: %v", err)
					time.Sleep(100 * time.Millisecond)
				}
				t.Logf("AC observable convergence endpoint=%s duration=%s", read.Conn.Target(), time.Since(start))
				time.Sleep(100 * time.Millisecond)
				stable, err := action(ctx, read, instance, blobKey(h))
				require.NoError(t, err)
				require.True(t, proto.Equal(ar, stable), "converged AC changed without a write")
				require.Len(t, ar.OutputFiles, 2)
				seen := map[string]bool{}
				if read.Conn.Target() == app.Conn.Target() {
					cacheMetadata(t, ctx, read, instance, blobKey(h), true)
				}
				uiMetadata(t, ctx, f, app, instance, blobKey(h), "AC")
				acBytes, err := proto.Marshal(ar)
				require.NoError(t, err)
				httpContent(t, ctx, f, instance, blobKey(h), true, acBytes)
				for _, o := range ar.OutputFiles {
					require.False(t, seen[o.Path], "duplicate output path %s", o.Path)
					seen[o.Path] = true
					content, err := read.Read(ctx, instance, o.Digest)
					require.NoError(t, err)
					httpContent(t, ctx, f, instance, o.Digest, false, content)
					casSurfaces(t, ctx, f, read, app, cli, o.Digest, content)
					uiMetadata(t, ctx, f, app, instance, o.Digest, "CAS")
					switch o.Path {
					case "_bb_ociregistry_blob_":
						require.Equal(t, d.Hash, o.Digest.Hash)
						require.Equal(t, d.SizeBytes, o.Digest.SizeBytes)
						require.Equal(t, model, content)
					case "_bb_ociregistry_blob_metadata_":
						require.Equal(t, field(number(nil, 1, uint64(d.SizeBytes)), 2, string(blobMedia)), content)
						s, v := fields(t, content)
						require.Equal(t, uint64(d.SizeBytes), v[1])
						meta, mv := fields(t, fetch(t, ctx, read, "FetchBlobMetadata", bref))
						require.Equal(t, v[1], mv[1])
						require.Equal(t, s[2], meta[2])
						require.Equal(t, blobMedia, meta[2])
					default:
						t.Fatalf("unknown output %q", o.Path)
					}
				}

			}
			manifestSurfaces(t, ctx, f, app, clients, raw, media)
			uiMetadata(t, ctx, f, app, instance, d, "CAS")
			mirrorContent(t, ctx, f, "blobs", manifest.Config.Digest, model)
		}
	}
	var config struct {
		Architecture string
		OS           string
	}
	require.NoError(t, json.Unmarshal(model, &config))
	require.Equal(t, "amd64", config.Architecture)
	require.Equal(t, "linux", config.OS)
}

func resource(inst string, d *repb.Digest, ac bool) []byte {
	r := field(nil, 1, string(number(field(nil, 1, d.Hash), 2, uint64(d.SizeBytes))))
	r = field(r, 2, inst)
	ct := uint64(2)
	if ac {
		ct = 1
	}
	r = number(r, 4, ct)
	return number(r, 5, 1)
}

func cacheMetadata(t *testing.T, ctx context.Context, c *harness.Clients, inst string, d *repb.Digest, ac bool) {
	t.Helper()
	_, v := fields(t, call(t, ctx, c, "/cache.service.Cache/GetMetadata", field(nil, 2, string(resource(inst, d, ac)))))
	require.Equal(t, uint64(d.SizeBytes), v[5])
	require.Positive(t, v[2])
	require.Positive(t, v[3])
	require.Positive(t, v[4])
}

// Every CAS output has the same content model, including the metadata proto.
func casSurfaces(t *testing.T, ctx context.Context, f *harness.Fixture, c, app *harness.Clients, cli string, d *repb.Digest, expected []byte) {
	t.Helper()
	require.Equal(t, d.Hash, hash(expected))
	require.Equal(t, d.SizeBytes, int64(len(expected)))
	missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Empty(t, missing.MissingBlobDigests)
	batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
	require.NoError(t, err)
	require.Len(t, batch.Responses, 1)
	require.True(t, proto.Equal(d, batch.Responses[0].Digest))
	require.Equal(t, int32(codes.OK), batch.Responses[0].GetStatus().GetCode())
	require.Equal(t, expected, batch.Responses[0].Data)
	cacheMetadata(t, ctx, c, instance, d, false)
	uri := "bytestream://localhost:1985/" + instance + "/blobs/" + d.Hash + "/" + strconv.FormatInt(d.SizeBytes, 10)
	ranged, _ := fields(t, call(t, ctx, app, "/api.v1.ApiService/GetFileRange", number(number(field(nil, 1, uri), 2, 1), 3, uint64(len(expected)))))
	require.Equal(t, expected[1:], ranged[1])
	cmd := exec.CommandContext(ctx, cli, "download", "--target=grpc://"+c.Conn.Target(), "--api_key="+f.APIKey, d.Hash+"/"+strconv.FormatInt(d.SizeBytes, 10))
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	downloaded, err := cmd.Output()
	require.NoError(t, err, stderr.String())
	require.Equal(t, expected, downloaded)
}

func absent(t *testing.T, ctx context.Context, f *harness.Fixture, cli string, c *harness.Clients, inst string, d *repb.Digest, ac bool) {
	t.Helper()
	req := field(nil, 2, string(resource(inst, d, ac)))
	var resp []byte
	err := c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", &req, &resp, grpc.ForceCodec(wireCodec{}))
	require.Equal(t, codes.NotFound, status.Code(err), "metadata must not persist a record")
	if ac {
		_, err = action(ctx, c, inst, d)
		require.Equal(t, codes.NotFound, status.Code(err))
	} else {
		_, err = c.Read(ctx, inst, d)
		require.Equal(t, codes.NotFound, status.Code(err))
		b, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: inst, Digests: []*repb.Digest{d}})
		require.NoError(t, err)
		require.Len(t, b.Responses, 1)
		require.True(t, proto.Equal(d, b.Responses[0].Digest))
		require.Equal(t, int32(codes.NotFound), b.Responses[0].GetStatus().GetCode())
	}
	if c.Conn.Target() == f.Clients(harness.App).Conn.Target() {
		err = c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", &req, &resp, grpc.ForceCodec(wireCodec{}))
		require.Equal(t, codes.NotFound, status.Code(err))
		ct := "CAS"
		if ac {
			ct = "AC"
		}
		_, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "instanceName": inst, "cacheType": ct, "digestFunction": "SHA256"}})
		require.Error(t, err)
		require.Contains(t, strings.ToLower(err.Error()), "not found")
		scheme, part := "bytestream", "blobs"
		expectedStatus := http.StatusBadRequest // missing CAS falls back to invocation artifacts, without an invocation ID
		if ac {
			scheme, part, expectedStatus = "actioncache", "blobs/ac", http.StatusNotFound
		}
		q := url.Values{}
		q.Set("bytestream_url", scheme+"://localhost:1985/"+inst+"/"+part+"/"+d.Hash+"/"+strconv.FormatInt(d.SizeBytes, 10))
		q.Set("request_context", base64.StdEncoding.EncodeToString(field(nil, 2, f.GroupID)))
		for _, endpoint := range []string{"/file/download", "/file/view"} {
			request, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+endpoint+"?"+q.Encode(), nil)
			require.NoError(t, err)
			response, err := f.Web.Client.Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			require.NoError(t, err)
			require.Equal(t, expectedStatus, response.StatusCode, string(body))
		}
	}
	if !ac {
		cmd := exec.CommandContext(ctx, cli, "download", "--target=grpc://"+c.Conn.Target(), "--api_key="+f.APIKey, d.Hash+"/"+strconv.FormatInt(d.SizeBytes, 10))
		cmd.Dir = t.TempDir()
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		result, err := cmd.CombinedOutput()
		require.Error(t, err, "CLI must not return an absent blob")
		require.Contains(t, strings.ToLower(string(result)), "notfound")
	}

}

func mirrorContent(t *testing.T, ctx context.Context, f *harness.Fixture, kind, digest string, expected []byte) {
	t.Helper()
	for _, method := range []string{"GET", "HEAD"} {
		req, err := http.NewRequestWithContext(ctx, method, f.Web.URL+"/v2/"+repo+"/"+kind+"/"+digest, nil)
		require.NoError(t, err)
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		require.Equal(t, strconv.Itoa(len(expected)), resp.Header.Get("Content-Length"))
		require.Equal(t, digest, resp.Header.Get("Docker-Content-Digest"))
		if method == "GET" {
			require.Equal(t, expected, body)
		} else {
			require.Empty(t, body)
		}
	}
}

func manifestSurfaces(t *testing.T, ctx context.Context, f *harness.Fixture, app *harness.Clients, clients []*harness.Clients, raw []byte, media string) {
	t.Helper()
	for _, read := range clients {
		ar, err := action(ctx, read, instance+"_manifest_content_", manifestKey())
		require.NoError(t, err)
		require.Len(t, ar.GetExecutionMetadata().GetAuxiliaryMetadata(), 1)
		a := ar.GetExecutionMetadata().GetAuxiliaryMetadata()[0]
		if read.Conn.Target() == app.Conn.Target() {
			cacheMetadata(t, ctx, read, instance+"_manifest_content_", manifestKey(), true)
		}
		acBytes, err := proto.Marshal(ar)
		require.NoError(t, err)
		httpContent(t, ctx, f, instance+"_manifest_content_", manifestKey(), true, acBytes)
		require.Equal(t, "type.googleapis.com/ociregistry.OCIManifestContent", a.TypeUrl)
		af, _ := fields(t, a.Value)
		require.Equal(t, raw, af[1])
		require.Equal(t, media, string(af[2]))
	}
	uiMetadata(t, ctx, f, app, instance+"_manifest_content_", manifestKey(), "AC")
}
