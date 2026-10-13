package encryption_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// Wire numbers follow the released encryption.proto, cache.proto and resource.proto.
// This codec only encodes valid requests; it avoids a source-built server dependency.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func msg(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func num(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func join(bs ...[]byte) []byte {
	var out []byte
	for _, b := range bs {
		out = append(out, b...)
	}
	return out
}
func field(b []byte, want protowire.Number) ([]byte, uint64) {
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("bad response tag")
		}
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				panic("bad response bytes")
			}
			if n == want {
				return v, 0
			}
			b = b[k:]
		} else if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				panic("bad response varint")
			}
			if n == want {
				return nil, v
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			if k < 0 {
				panic("bad response field")
			}
			b = b[k:]
		}
	}
	return nil, 0
}
func rpc(t *testing.T, ctx context.Context, c *harness.Clients, method string, req []byte) []byte {
	t.Helper()
	var out []byte
	require.NoError(t, c.Conn.Invoke(ctx, method, req, &out, grpc.ForceCodec(wireCodec{})))
	return out
}

const service = "/buildbuddy.service.BuildBuddyService/"

type model struct {
	enabled bool
	key     []byte
}

func TestEncryptionState(t *testing.T) {
	kms := t.TempDir()
	for _, name := range []string{"master", "customer"} {
		require.NoError(t, os.WriteFile(filepath.Join(kms, name), []byte("0123456789abcdef0123456789abcdef"), 0600))
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, DockerArgs: map[harness.Component][]string{harness.App: {"--volume", kms + ":/kms:ro"}}, Config: map[harness.Component]harness.Config{
		harness.App:        {"auth": harness.Config{"api_key_group_cache_ttl": "0s"}, "keystore": harness.Config{"master_key_uri": "local-insecure-kms://master", "local_insecure_kms_directory": "/kms"}, "app": harness.Config{"client_identity": harness.Config{"key": "fixture-identity"}}, "crypter": harness.Config{"permitted_clients": []string{"cache-proxy"}, "key_ttl": "100ms"}},
		harness.CacheProxy: {"auth": harness.Config{"remote": harness.Config{"jwt_cache_ttl": "1ms"}}, "app": harness.Config{"client_identity": harness.Config{"key": "fixture-identity", "client": "cache-proxy", "origin": "fixture"}}, "crypter": harness.Config{"remote_target": "grpc://app:1985", "key_ttl": "100ms"}},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	f.Web.Client.Timeout = 10 * time.Second
	f.APIKey = f.Web.CreateAPIKey(t, f.GroupID, "encryption-admin", []string{"CACHE_WRITE", "ORG_ADMIN"})
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"Client": "cache-proxy", "Origin": "fixture", "exp": time.Now().Add(5 * time.Minute).Unix()}).SignedString([]byte("fixture-identity"))
	require.NoError(t, err)
	keyCtx := metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-client-identity", token)
	rc := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)}
	m := model{}
	var previousKey []byte
	epochBlobs := map[*repb.Digest][]byte{}
	check := func() {
		out := rpc(t, ctx, app, service+"GetEncryptionConfig", nil)
		_, enabled := field(out, 2)
		require.Equal(t, m.enabled, enabled == 1)
		web, err := f.Web.RPC("GetEncryptionConfig", rc)
		require.NoError(t, err)
		require.Equal(t, m.enabled, web["enabled"] == true)
		supported, _ := field(out, 3)
		require.Contains(t, supported, byte(1), "local KMS must be advertised")
		if !m.enabled {
			require.Eventually(t, func() bool {
				var out []byte
				err := app.Conn.Invoke(keyCtx, "/encryption.EncryptionService/GetEncryptionKey", []byte(nil), &out, grpc.ForceCodec(wireCodec{}))
				return status.Code(err) == codes.NotFound
			}, 15*time.Second, 25*time.Millisecond, "disabled group must eventually have no active key")
		}
		if m.enabled {
			require.Eventually(t, func() bool {
				var out []byte
				err := app.Conn.Invoke(keyCtx, "/encryption.EncryptionService/GetEncryptionKey", []byte(nil), &out, grpc.ForceCodec(wireCodec{}))
				key, _ := field(out, 1)
				return err == nil && (previousKey == nil || string(key) != string(previousKey))
			}, 15*time.Second, 25*time.Millisecond, "enabled group must expose its current key")
			for i := 0; i < 2; i++ {
				out := rpc(t, keyCtx, app, "/encryption.EncryptionService/GetEncryptionKey", nil)
				key, _ := field(out, 1)
				md, _ := field(key, 1)
				id, _ := field(md, 1)
				_, version := field(md, 2)
				material, _ := field(key, 3)
				require.NotEmpty(t, id)
				require.Equal(t, uint64(1), version)
				require.Len(t, material, 32)
				if m.key == nil {
					m.key = key
				}
				require.Equal(t, m.key, key)
				require.Equal(t, out, rpc(t, keyCtx, app, "/encryption.EncryptionService/GetEncryptionKey", msg(1, md)))
			}
		}
	}
	check()
	for step, enabled := range []bool{false, true, true, false, false, true, true} {
		t.Logf("step %d enabled=%v", step, enabled)
		req := num(2, 0)
		if enabled {
			req = join(num(2, 1), msg(3, msg(1, msg(1, []byte("customer")))))
		}
		rpc(t, ctx, app, service+"SetEncryptionConfig", req)
		if m.enabled != enabled {
			if !enabled {
				previousKey = append([]byte(nil), m.key...)
			}
			m.key = nil
			epochBlobs = map[*repb.Digest][]byte{}
		}
		m.enabled = enabled
		check()
		if enabled && previousKey != nil {
			require.NotEqual(t, previousKey, m.key, "re-enable creates a new key")
		}
		// Unique content avoids deduplication with records from another encryption epoch.
		data := []byte(fmt.Sprintf("encryption-state step %d enabled %v", step, enabled))
		writer := proxy
		if step%2 == 0 {
			writer = app
		}
		d := harness.Digest(data)
		if step%3 == 0 {
			r, err := writer.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: data}}})
			require.NoError(t, err)
			require.Len(t, r.Responses, 1)
			require.Zero(t, r.Responses[0].Status.GetCode())
		} else {
			uploaded, err := writer.Upload(ctx, "", data)
			require.NoError(t, err)
			require.True(t, proto.Equal(d, uploaded))
		}
		epochBlobs[d] = data
		for digest, content := range epochBlobs {
			for _, c := range []*harness.Clients{app, proxy} {
				got, err := c.Read(ctx, "", digest)
				require.NoError(t, err)
				require.Equal(t, content, got)
			}
		}
		for _, c := range []*harness.Clients{app, proxy} {
			require.Eventually(t, func() bool {
				r, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
				return e == nil && len(r.MissingBlobDigests) == 0
			}, 10*time.Second, 25*time.Millisecond, "cache write must become visible")
			got, err := c.Read(ctx, "", d)
			require.NoError(t, err)
			require.Equal(t, data, got)
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
			require.NoError(t, err)
			require.Empty(t, missing.MissingBlobDigests)
			batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}})
			require.NoError(t, err)
			require.Len(t, batch.Responses, 1)
			require.Equal(t, int32(0), batch.Responses[0].Status.GetCode())
			require.Equal(t, data, batch.Responses[0].Data)
		}
		digestWire, err := proto.Marshal(d)
		require.NoError(t, err)
		resource := join(msg(1, digestWire), num(4, 2))
		mdReq := msg(2, resource)
		md := rpc(t, ctx, app, "/cache.service.Cache/GetMetadata", mdReq)
		_, size := field(md, 5)
		proxyMD := rpc(t, ctx, proxy, "/cache.service.Cache/GetMetadata", mdReq)
		_, proxySize := field(proxyMD, 5)
		require.Equal(t, size, proxySize)
		_, stored := field(md, 2)
		if m.enabled {
			require.Greater(t, stored, uint64(len(data)), "new app record must contain encryption overhead")
		}
		require.Equal(t, uint64(len(data)), size)
		ui := rpc(t, ctx, app, service+"GetCacheMetadata", mdReq)
		_, uiSize := field(ui, 5)
		require.Equal(t, size, uiSize)
		web, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "cacheType": "CAS"}})
		require.NoError(t, err)
		require.Equal(t, fmt.Sprint(d.SizeBytes), fmt.Sprint(web["digestSizeBytes"]))
		action := harness.Digest([]byte(fmt.Sprintf("action-%d", step)))
		for update := 0; update < 2; update++ {
			want := &repb.ActionResult{ExitCode: 0, StdoutRaw: []byte(fmt.Sprintf("result-%d-%d", step, update))}
			acWriter := proxy
			if update == 0 {
				acWriter = app
			}
			for repeat := 0; repeat < 2; repeat++ {
				_, err := acWriter.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: action, ActionResult: want})
				require.NoError(t, err)
			}
			for _, c := range []*harness.Clients{app, proxy} {
				got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: action, InlineStdout: true})
				require.NoError(t, err)
				require.Equal(t, want.StdoutRaw, got.StdoutRaw)
			}
		}
		bb, err := exec.LookPath("bb")
		require.NoError(t, err, "runner must provide released bb CLI")
		cmd := exec.CommandContext(ctx, bb, "download", fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes), "--target=grpc://"+f.Endpoint(harness.CacheProxy).GRPC, "--api_key="+f.APIKey)
		got, err := cmd.Output()
		require.NoError(t, err)
		require.Equal(t, data, got)
		check()
	}
}
