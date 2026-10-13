package encryption_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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
	"google.golang.org/protobuf/encoding/protojson"
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

// Records are retained across transitions. Encryption key ID is part of the
// released Pebble namespace, so plaintext records reappear after disable;
// deleted encrypted namespaces never reappear after re-enable.
type blob struct {
	d         *repb.Digest
	data      []byte
	namespace string
	dir       *repb.Directory
}
type actionRecord struct {
	d         *repb.Digest
	result    *repb.ActionResult
	namespace string
}
type suite struct {
	t          *testing.T
	ctx        context.Context
	f          *harness.Fixture
	clients    []*harness.Clients
	keyCtx     context.Context
	namespace  string
	currentKey []byte
	stable     map[string]string
	stableMu   sync.Mutex
	blobs      []blob
	actions    []actionRecord
	oldKeys    [][]byte
}

func startSuite(t *testing.T, fast bool) *suite {
	kms := t.TempDir()
	for _, name := range []string{"master", "customer"} {
		require.NoError(t, os.WriteFile(filepath.Join(kms, name), []byte("0123456789abcdef0123456789abcdef"), 0600))
	}
	appConfig := harness.Config{"keystore": harness.Config{"master_key_uri": "local-insecure-kms://master", "local_insecure_kms_directory": "/kms"}, "app": harness.Config{"client_identity": harness.Config{"key": "fixture-identity"}}, "crypter": harness.Config{"permitted_clients": []string{"cache-proxy"}}}
	proxyConfig := harness.Config{"app": harness.Config{"client_identity": harness.Config{"key": "fixture-identity", "client": "cache-proxy", "origin": "fixture"}}, "crypter": harness.Config{"remote_target": "grpc://app:1985"}}
	if fast {
		appConfig["auth"] = harness.Config{"api_key_group_cache_ttl": "0s"}
		appConfig["crypter"].(harness.Config)["key_ttl"] = "100ms"
		proxyConfig["auth"] = harness.Config{"remote": harness.Config{"jwt_cache_ttl": "1ms"}}
		proxyConfig["crypter"].(harness.Config)["key_ttl"] = "100ms"
	}
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, DockerArgs: map[harness.Component][]string{harness.App: {"--volume", kms + ":/kms:ro"}}, Config: map[harness.Component]harness.Config{harness.App: appConfig, harness.CacheProxy: proxyConfig}})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	f.Web.Client.Timeout = 15 * time.Second
	f.APIKey = f.Web.CreateAPIKey(t, f.GroupID, "encryption-admin", []string{"CACHE_WRITE", "ORG_ADMIN"})
	second := f.Web.CreateAPIKey(t, f.GroupID, "encryption-peer", []string{"CACHE_WRITE", "ORG_ADMIN"})
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"Client": "cache-proxy", "Origin": "fixture", "exp": time.Now().Add(15 * time.Minute).Unix()}).SignedString([]byte("fixture-identity"))
	require.NoError(t, err)
	return &suite{t: t, ctx: ctx, f: f, clients: []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy), harness.Dial(t, f.Endpoint(harness.App).GRPC, second), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, second)}, keyCtx: metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-client-identity", token)}
}
func (s *suite) invoke(c *harness.Clients, method string, req []byte) ([]byte, error) {
	var out []byte
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	err := c.Conn.Invoke(ctx, method, req, &out, grpc.ForceCodec(wireCodec{}))
	return out, err
}
func (s *suite) httpFile(route, scheme string, d *repb.Digest) ([]byte, int) {
	path := "blobs/"
	if scheme == "actioncache" {
		path = "blobs/ac/"
	}
	q := url.Values{"bytestream_url": {fmt.Sprintf("%s://app:1985/%s%s/%d", scheme, path, d.Hash, d.SizeBytes)}, "request_context": {base64.StdEncoding.EncodeToString(msg(2, []byte(s.f.GroupID)))}}
	req, err := http.NewRequestWithContext(s.ctx, "GET", s.f.Web.URL+"/file/"+route+"?"+q.Encode(), nil)
	require.NoError(s.t, err)
	resp, err := s.f.Web.Client.Do(req)
	require.NoError(s.t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	return data, resp.StatusCode
}
func (s *suite) webProto(method string, req []byte, stream bool) []byte {
	ct := "application/protobuf"
	if stream {
		ct = "application/proto+prefixed"
		prefix := make([]byte, 5)
		binary.BigEndian.PutUint32(prefix[1:], uint32(len(req)))
		req = append(prefix, req...)
	}
	req = append(req, msg(1, msg(2, []byte(s.f.GroupID)))...)
	// Prefix length must include request context.
	if stream {
		binary.BigEndian.PutUint32(req[1:5], uint32(len(req)-5))
	}
	r, err := http.NewRequestWithContext(s.ctx, "POST", s.f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(req))
	require.NoError(s.t, err)
	r.Header.Set("Content-Type", ct)
	resp, err := s.f.Web.Client.Do(r)
	require.NoError(s.t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	require.Equal(s.t, 200, resp.StatusCode, "%s: %s", method, data)
	return data
}
func wireMessages(b []byte, n protowire.Number) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		tag, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("tag")
		}
		b = b[k:]
		k = protowire.ConsumeFieldValue(tag, typ, b)
		if k < 0 {
			panic("value")
		}
		if tag == n && typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(b)
			out = append(out, v)
		}
		b = b[k:]
	}
	return out
}
func (s *suite) converge(label string, bound time.Duration, predicate func() bool) {
	start := time.Now()
	deadline := start.Add(bound)
	for !predicate() {
		require.True(s.t, time.Now().Before(deadline), "%s convergence exceeded %s", label, bound)
		delay := 100 * time.Millisecond
		if label == "default auth caches encryption visibility" {
			delay = time.Second
		}
		time.Sleep(delay)
	}
	recordSurface := len(label) >= 3 && (label[:3] == "CAS" || label[:3] == "AC ")
	s.stableMu.Lock()
	if s.stable == nil {
		s.stable = make(map[string]string)
	}
	previous, ok := s.stable[label]
	s.stableMu.Unlock()
	if recordSurface && ok && previous == s.namespace {
		// Unchanged retained records still undergo every surface assertion below.
		// A fresh convergence window is needed only when their state changes.
		return
	}
	s.t.Logf("%s converged in %s", label, time.Since(start))
	// Three additional samples; key transitions span the accelerated cache TTLs.
	interval := 25 * time.Millisecond
	if label == "encryption transition" || label == "deleted explicit key" || label == "default auth caches encryption visibility" {
		interval = 200 * time.Millisecond
	}
	for i := 0; i < 3; i++ {
		time.Sleep(interval)
		require.True(s.t, predicate(), "%s convergence was unstable", label)
	}
	if recordSurface {
		s.stableMu.Lock()
		s.stable[label] = s.namespace
		s.stableMu.Unlock()
	}
}
func (s *suite) config(enabled bool, bound time.Duration) {
	t := s.t
	req := num(2, 0)
	if enabled {
		req = join(num(2, 1), msg(3, msg(1, msg(1, []byte("customer")))))
	}
	if !enabled && s.namespace != "" {
		out := rpc(t, s.keyCtx, s.clients[0], "/encryption.EncryptionService/GetEncryptionKey", nil)
		k, _ := field(out, 1)
		s.oldKeys = append(s.oldKeys, k)
	}
	rpc(t, s.ctx, s.clients[0], service+"SetEncryptionConfig", req)
	var key []byte
	s.converge("encryption transition", bound, func() bool {
		for _, c := range []*harness.Clients{s.clients[0], s.clients[2]} {
			out, err := s.invoke(c, service+"GetEncryptionConfig", nil)
			_, v := field(out, 2)
			supported, _ := field(out, 3)
			if err != nil || (v == 1) != enabled || !bytes.Contains(supported, []byte{1}) {
				return false
			}
		}
		var response []byte
		e := s.clients[0].Conn.Invoke(s.keyCtx, "/encryption.EncryptionService/GetEncryptionKey", []byte(nil), &response, grpc.ForceCodec(wireCodec{}))
		if !enabled {
			return status.Code(e) == codes.NotFound
		}
		key, _ = field(response, 1)
		md, _ := field(key, 1)
		id, _ := field(md, 1)
		for _, old := range s.oldKeys {
			oldMD, _ := field(old, 1)
			oldID, _ := field(oldMD, 1)
			if bytes.Equal(id, oldID) {
				return false
			}
		}
		return e == nil && len(id) > 0
	})
	s.namespace = ""
	s.currentKey = key
	if enabled {
		md, _ := field(key, 1)
		id, _ := field(md, 1)
		_, version := field(md, 2)
		material, _ := field(key, 3)
		require.Equal(t, uint64(1), version)
		require.Len(t, material, 32)
		s.namespace = string(id)
		require.Equal(t, msg(1, key), rpc(t, s.keyCtx, s.clients[0], "/encryption.EncryptionService/GetEncryptionKey", msg(1, md)))
	}
	for _, old := range s.oldKeys {
		md, _ := field(old, 1)
		s.converge("deleted explicit key", bound, func() bool {
			var response []byte
			err := s.clients[0].Conn.Invoke(s.keyCtx, "/encryption.EncryptionService/GetEncryptionKey", msg(1, md), &response, grpc.ForceCodec(wireCodec{}))
			return status.Code(err) == codes.NotFound
		})
	}
	web, err := s.f.Web.RPC("GetEncryptionConfig", map[string]any{"requestContext": s.f.Web.RequestContext(s.f.GroupID)})
	require.NoError(t, err)
	require.Equal(t, enabled, web["enabled"] == true)
	_, v := field(s.webProto("GetEncryptionConfig", nil, false), 2)
	require.Equal(t, enabled, v == 1)
	s.snapshot()
}
func (s *suite) metadata(d *repb.Digest, cacheType uint64, logical int64) {
	t := s.t
	dw, err := proto.Marshal(d)
	require.NoError(t, err)
	req := msg(2, join(msg(1, dw), num(4, cacheType)))
	var appMD []byte
	for i, c := range s.clients {
		md, err := s.invoke(c, "/cache.service.Cache/GetMetadata", req)
		if cacheType == 1 && (i == 1 || i == 3) {
			// Known cross-surface divergence, isolated by ac_metadata_bug_test.
			// Assert the observed error, rather than omitting this surface.
			require.Equal(t, codes.NotFound, status.Code(err), "documented proxy AC metadata divergence changed")
			continue
		}
		require.NoError(t, err)
		_, size := field(md, 5)
		_, stored := field(md, 2)
		_, access := field(md, 3)
		_, modify := field(md, 4)
		require.Equal(t, uint64(logical), size)
		require.Positive(t, stored)
		require.Positive(t, modify)
		require.GreaterOrEqual(t, access, modify)
		require.LessOrEqual(t, access, uint64(time.Now().Add(time.Second).UnixMicro()))
		if s.namespace != "" && cacheType == 2 && logical <= 100 {
			require.Greater(t, stored, uint64(logical))
		}
		if i == 0 {
			appMD = md
		}
	}
	for _, md := range [][]byte{rpc(t, s.ctx, s.clients[0], service+"GetCacheMetadata", req), s.webProto("GetCacheMetadata", req, false)} {
		for _, n := range []protowire.Number{2, 4, 5} {
			_, a := field(appMD, n)
			_, b := field(md, n)
			require.Equal(t, a, b, "metadata field %d", n)
		}
		_, access := field(md, 3)
		_, appAccess := field(appMD, 3)
		require.GreaterOrEqual(t, access, appAccess)
		require.LessOrEqual(t, access, uint64(time.Now().Add(time.Second).UnixMicro()))
	}
	typ := "CAS"
	if cacheType == 1 {
		typ = "AC"
	}
	web, err := s.f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": s.f.Web.RequestContext(s.f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "cacheType": typ}})
	require.NoError(t, err)
	for name, n := range map[string]protowire.Number{"storedSizeBytes": 2, "lastModifyUsec": 4, "digestSizeBytes": 5} {
		_, v := field(appMD, n)
		require.Equal(t, fmt.Sprint(v), fmt.Sprint(web[name]), name)
	}
	var access uint64
	_, err = fmt.Sscan(fmt.Sprint(web["lastAccessUsec"]), &access)
	require.NoError(t, err)
	_, appAccess := field(appMD, 3)
	require.GreaterOrEqual(t, access, appAccess)
	require.LessOrEqual(t, access, uint64(time.Now().Add(time.Second).UnixMicro()))
}
func (s *suite) cli(d *repb.Digest, endpoint int, args ...string) []byte {
	bb, err := exec.LookPath("bb")
	require.NoError(s.t, err)
	target := harness.App
	if endpoint == 1 {
		target = harness.CacheProxy
	}
	cmdArgs := append([]string{"download", fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes), "--target=grpc://" + s.f.Endpoint(target).GRPC, "--api_key=" + s.f.APIKey}, args...)
	cmd := exec.CommandContext(s.ctx, bb, cmdArgs...)
	out, err := cmd.Output()
	require.NoError(s.t, err)
	return out
}
func (s *suite) cliMissing(d *repb.Digest, endpoint int) {
	bb, err := exec.LookPath("bb")
	require.NoError(s.t, err)
	target := harness.App
	if endpoint == 1 {
		target = harness.CacheProxy
	}
	cmd := exec.CommandContext(s.ctx, bb, "download", fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes), "--target=grpc://"+s.f.Endpoint(target).GRPC, "--api_key="+s.f.APIKey)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.Error(s.t, err)
	require.Empty(s.t, out)
	require.Contains(s.t, stderr.String(), "NotFound")
}

func (s *suite) tree(b blob) {
	t := s.t
	expected := map[string]*repb.Directory{}
	files := map[string]blob{}
	var walk func(blob, string) int64
	walk = func(record blob, path string) int64 {
		expected[record.d.Hash] = record.dir
		size := record.d.SizeBytes
		for _, file := range record.dir.Files {
			size += file.Digest.SizeBytes
			for _, candidate := range s.blobs {
				if candidate.namespace == s.namespace && proto.Equal(candidate.d, file.Digest) {
					files[filepath.Join(path, file.Name)] = candidate
				}
			}
		}
		for _, child := range record.dir.Directories {
			found := false
			for _, candidate := range s.blobs {
				if candidate.namespace == s.namespace && proto.Equal(candidate.d, child.Digest) {
					size += walk(candidate, filepath.Join(path, child.Name))
					found = true
					break
				}
			}
			require.True(t, found)
		}
		return size
	}
	total := walk(b, "")
	compare := func(dirs []*repb.Directory) {
		require.Len(t, dirs, len(expected))
		for _, dir := range dirs {
			data, err := proto.Marshal(dir)
			require.NoError(t, err)
			require.True(t, proto.Equal(expected[harness.Digest(data).Hash], dir), "tree directory differs from model")
		}
	}
	for _, c := range s.clients {
		stream, err := c.CAS.GetTree(s.ctx, &repb.GetTreeRequest{RootDigest: b.d})
		require.NoError(t, err)
		var dirs []*repb.Directory
		for {
			r, e := stream.Recv()
			if e == io.EOF {
				break
			}
			require.NoError(t, e)
			dirs = append(dirs, r.Directories...)
		}
		compare(dirs)
	}
	dw, err := proto.Marshal(b.d)
	require.NoError(t, err)
	stream, err := s.clients[0].Conn.NewStream(s.ctx, &grpc.StreamDesc{ServerStreams: true}, service+"GetTree", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(msg(3, dw)))
	require.NoError(t, stream.CloseSend())
	var dirs []*repb.Directory
	decode := func(response []byte) {
		for _, wire := range wireMessages(response, 2) {
			d := &repb.Directory{}
			require.NoError(t, proto.Unmarshal(wire, d))
			dirs = append(dirs, d)
		}
	}
	for {
		var response []byte
		err := stream.RecvMsg(&response)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		decode(response)
	}
	compare(dirs)
	frames := s.webProto("GetTree", msg(3, dw), true)
	dirs = nil
	for len(frames) > 0 {
		require.GreaterOrEqual(t, len(frames), 5)
		n := int(binary.BigEndian.Uint32(frames[1:5]))
		require.GreaterOrEqual(t, len(frames), 5+n)
		if frames[0] == 0 {
			decode(frames[5 : 5+n])
		} else {
			require.Equal(t, byte(0x80), frames[0])
			require.Contains(t, string(frames[5:5+n]), "Grpc-Status: 0")
		}
		frames = frames[5+n:]
	}
	compare(dirs)
	for _, response := range [][]byte{rpc(t, s.ctx, s.clients[0], service+"GetTreeDirectorySizes", msg(2, dw)), s.webProto("GetTreeDirectorySizes", msg(2, dw), false)} {
		sizes := wireMessages(response, 2)
		require.Len(t, sizes, len(expected))
		found := false
		for _, entry := range sizes {
			hash, _ := field(entry, 1)
			_, size := field(entry, 2)
			_, children := field(entry, 3)
			if string(hash) == b.d.Hash {
				require.Equal(t, uint64(total), size)
				require.Equal(t, uint64(len(files)+len(expected)-1), children)
				found = true
			}
		}
		require.True(t, found)
	}
	web, err := s.f.Web.RPC("GetTreeDirectorySizes", map[string]any{"requestContext": s.f.Web.RequestContext(s.f.GroupID), "rootDigest": map[string]any{"hash": b.d.Hash, "sizeBytes": fmt.Sprint(b.d.SizeBytes)}})
	require.NoError(t, err)
	sizes, ok := web["sizes"].([]any)
	require.True(t, ok)
	require.Len(t, sizes, len(expected))
	found := false
	for _, entry := range sizes {
		size := entry.(map[string]any)
		if size["digest"] == b.d.Hash {
			require.Equal(t, fmt.Sprint(total), fmt.Sprint(size["totalSize"]))
			require.Equal(t, fmt.Sprint(len(files)+len(expected)-1), fmt.Sprint(size["childCount"]))
			found = true
		}
	}
	require.True(t, found)
	for endpoint := 0; endpoint < 2; endpoint++ {
		out := s.cli(b.d, endpoint, "--type=Directory")
		dir := &repb.Directory{}
		require.NoError(t, protojson.Unmarshal(out, dir))
		require.True(t, proto.Equal(b.dir, dir))
		dest := t.TempDir()
		s.cli(b.d, endpoint, "--output_directory="+dest)
		for path, file := range files {
			data, err := os.ReadFile(filepath.Join(dest, path))
			require.NoError(t, err)
			require.Equal(t, file.data, data)
			info, err := os.Stat(filepath.Join(dest, path))
			require.NoError(t, err)
			require.NotZero(t, info.Mode()&0111, "executable output mode")
		}
	}
}
func (s *suite) missingTree(b blob) {
	t := s.t
	// Known REv2 status divergence isolated by tree_status_bug_test.
	for _, c := range s.clients {
		stream, err := c.CAS.GetTree(s.ctx, &repb.GetTreeRequest{RootDigest: b.d})
		require.NoError(t, err)
		_, err = stream.Recv()
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	}
	dw, err := proto.Marshal(b.d)
	require.NoError(t, err)
	stream, err := s.clients[0].Conn.NewStream(s.ctx, &grpc.StreamDesc{ServerStreams: true}, service+"GetTree", grpc.ForceCodec(wireCodec{}))
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(msg(3, dw)))
	require.NoError(t, stream.CloseSend())
	var out []byte
	err = stream.RecvMsg(&out)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	body := join(msg(3, dw), msg(1, msg(2, []byte(s.f.GroupID))))
	prefix := make([]byte, 5)
	binary.BigEndian.PutUint32(prefix[1:], uint32(len(body)))
	r, err := http.NewRequestWithContext(s.ctx, "POST", s.f.Web.URL+"/rpc/BuildBuddyService/GetTree", bytes.NewReader(append(prefix, body...)))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/proto+prefixed")
	response, err := s.f.Web.Client.Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, 200, response.StatusCode)
	if len(data) == 0 {
		require.Equal(t, "9", response.Header.Get("grpc-status"))
	} else {
		require.Equal(t, byte(0x80), data[0])
		require.Contains(t, string(data[5:]), "Grpc-Status: 9")
	}
	_, err = s.invoke(s.clients[0], service+"GetTreeDirectorySizes", msg(2, dw))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = s.f.Web.RPC("GetTreeDirectorySizes", map[string]any{"requestContext": s.f.Web.RequestContext(s.f.GroupID), "rootDigest": map[string]any{"hash": b.d.Hash, "sizeBytes": fmt.Sprint(b.d.SizeBytes)}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "FailedPrecondition")
}

func (s *suite) missingMetadata(d *repb.Digest, cacheType uint64) {
	dw, err := proto.Marshal(d)
	require.NoError(s.t, err)
	req := msg(2, join(msg(1, dw), num(4, cacheType)))
	for _, c := range s.clients {
		_, err := s.invoke(c, "/cache.service.Cache/GetMetadata", req)
		require.Equal(s.t, codes.NotFound, status.Code(err))
	}
	_, err = s.invoke(s.clients[0], service+"GetCacheMetadata", req)
	require.Equal(s.t, codes.NotFound, status.Code(err))
	typ := "CAS"
	if cacheType == 1 {
		typ = "AC"
	}
	_, err = s.f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": s.f.Web.RequestContext(s.f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "cacheType": typ}})
	require.Error(s.t, err)
}

func (s *suite) snapshot() {
	t := s.t
	enabled := s.namespace != ""
	for _, c := range []*harness.Clients{s.clients[0], s.clients[2]} {
		out, err := s.invoke(c, service+"GetEncryptionConfig", nil)
		require.NoError(t, err)
		_, v := field(out, 2)
		require.Equal(t, enabled, v == 1)
		var response []byte
		err = c.Conn.Invoke(s.keyCtx, "/encryption.EncryptionService/GetEncryptionKey", []byte(nil), &response, grpc.ForceCodec(wireCodec{}))
		if enabled {
			require.NoError(t, err)
			key, _ := field(response, 1)
			require.Equal(t, s.currentKey, key)
		} else {
			require.Equal(t, codes.NotFound, status.Code(err))
		}
	}
	for _, old := range s.oldKeys {
		md, _ := field(old, 1)
		for _, c := range []*harness.Clients{s.clients[0], s.clients[2]} {
			var response []byte
			err := c.Conn.Invoke(s.keyCtx, "/encryption.EncryptionService/GetEncryptionKey", msg(1, md), &response, grpc.ForceCodec(wireCodec{}))
			require.Equal(t, codes.NotFound, status.Code(err))
		}
	}
	web, err := s.f.Web.RPC("GetEncryptionConfig", map[string]any{"requestContext": s.f.Web.RequestContext(s.f.GroupID)})
	require.NoError(t, err)
	require.Equal(t, enabled, web["enabled"] == true)
	_, v := field(s.webProto("GetEncryptionConfig", nil, false), 2)
	require.Equal(t, enabled, v == 1)

	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, b := range s.blobs {
		limit <- struct{}{}
		wg.Add(1)
		go func(b blob) {
			defer wg.Done()
			defer func() { <-limit }()
			present := b.namespace == s.namespace
			s.converge("CAS "+b.d.Hash[:8], 15*time.Second, func() bool {
				for _, c := range s.clients {
					r, err := c.CAS.FindMissingBlobs(s.ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{b.d}})
					if err != nil || (len(r.MissingBlobDigests) == 0) != present {
						return false
					}
					data, err := c.Read(s.ctx, "", b.d)
					if present {
						if err != nil || !bytes.Equal(b.data, data) {
							return false
						}
					} else if status.Code(err) != codes.NotFound {
						return false
					}
				}
				return true
			})
			for _, c := range s.clients {
				data, err := c.Read(s.ctx, "", b.d)
				if present {
					require.NoError(t, err)
					require.Equal(t, b.data, data)
				} else {
					require.Equal(t, codes.NotFound, status.Code(err))
				}
				r, err := c.CAS.BatchReadBlobs(s.ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{b.d}})
				require.NoError(t, err)
				require.Len(t, r.Responses, 1)
				require.True(t, proto.Equal(b.d, r.Responses[0].Digest))
				if present {
					require.Zero(t, r.Responses[0].Status.GetCode())
					require.Equal(t, b.data, r.Responses[0].Data)
				} else {
					require.Equal(t, int32(codes.NotFound), r.Responses[0].Status.GetCode())
				}
			}
			for _, route := range []string{"download", "view"} {
				data, code := s.httpFile(route, "bytestream", b.d)
				if present {
					require.Equal(t, 200, code)
					require.Equal(t, b.data, data)
				} else {
					require.NotEqual(t, 200, code)
				}
			}
			if !present {
				s.missingMetadata(b.d, 2)
				if b.dir != nil {
					s.missingTree(b)
				}
				for endpoint := 0; endpoint < 2; endpoint++ {
					s.cliMissing(b.d, endpoint)
				}
			}
			if present {
				s.metadata(b.d, 2, b.d.SizeBytes)
				for endpoint := 0; endpoint < 2; endpoint++ {
					require.Equal(t, b.data, s.cli(b.d, endpoint))
				}
				if b.dir != nil {
					s.tree(b)
				}
			}
		}(b)
	}
	wg.Wait()
	require.False(t, t.Failed(), "retained CAS snapshot failed")
	for _, a := range s.actions {
		present := a.namespace == s.namespace
		resultBytes, err := proto.Marshal(a.result)
		require.NoError(t, err)
		s.converge("AC "+a.d.Hash[:8]+"/"+harness.Digest(resultBytes).Hash[:8], 15*time.Second, func() bool {
			for _, c := range s.clients {
				got, err := c.AC.GetActionResult(s.ctx, &repb.GetActionResultRequest{ActionDigest: a.d})
				if present {
					if err != nil || !proto.Equal(a.result, got) {
						return false
					}
				} else if status.Code(err) != codes.NotFound {
					return false
				}
			}
			return true
		})
		for _, route := range []string{"download", "view"} {
			data, code := s.httpFile(route, "actioncache", a.d)
			if present {
				require.Equal(t, 200, code)
				got := &repb.ActionResult{}
				require.NoError(t, proto.Unmarshal(data, got))
				require.True(t, proto.Equal(a.result, got), "complete AC HTTP mismatch")
			} else {
				require.NotEqual(t, 200, code)
			}
		}
		if present {
			for _, c := range s.clients {
				got, err := c.AC.GetActionResult(s.ctx, &repb.GetActionResultRequest{ActionDigest: a.d, InlineStdout: true, InlineStderr: true, InlineOutputFiles: []string{"output"}})
				require.NoError(t, err)
				want := proto.Clone(a.result).(*repb.ActionResult)
				for _, output := range want.OutputFiles {
					for _, b := range s.blobs {
						if b.namespace == s.namespace && proto.Equal(output.Digest, b.d) {
							output.Contents = b.data
						}
					}
				}
				require.True(t, proto.Equal(want, got), "inline AC result differs from model")
			}
		}
		if !present {
			s.missingMetadata(a.d, 1)
		}
		if present {
			s.metadata(a.d, 1, a.d.SizeBytes)
		}
	}
}
func (s *suite) write(step int, batch bool, dir bool) {
	data := []byte(fmt.Sprintf("payload-%d", step))
	var directory *repb.Directory
	if dir {
		previous := s.blobs[len(s.blobs)-1]
		fileDigest := previous.d
		if previous.dir != nil {
			fileDigest = previous.dir.Files[0].Digest
		}
		directory = &repb.Directory{Files: []*repb.FileNode{{Name: fmt.Sprintf("output-%d", step), Digest: fileDigest, IsExecutable: true}}}
		if previous.dir != nil {
			directory.Directories = []*repb.DirectoryNode{{Name: "subtree", Digest: previous.d}}
		}
		var err error
		data, err = proto.Marshal(directory)
		require.NoError(s.t, err)
	}
	d := harness.Digest(data)
	writer := s.clients[step%len(s.clients)]
	for repeat := 0; repeat < 2; repeat++ {
		if batch {
			r, err := writer.CAS.BatchUpdateBlobs(s.ctx, &repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: data}}})
			require.NoError(s.t, err)
			require.Len(s.t, r.Responses, 1)
			require.True(s.t, proto.Equal(d, r.Responses[0].Digest))
			require.Zero(s.t, r.Responses[0].Status.GetCode())
		} else {
			got, err := writer.Upload(s.ctx, "", data)
			require.NoError(s.t, err)
			require.True(s.t, proto.Equal(d, got))
		}
		if repeat == 0 {
			s.blobs = append(s.blobs, blob{d: d, data: data, namespace: s.namespace, dir: directory})
		}
		s.snapshot()
	}
}
func (s *suite) putProto(m proto.Message, writer int) *repb.Digest {
	data, err := proto.Marshal(m)
	require.NoError(s.t, err)
	d, err := s.clients[writer].Upload(s.ctx, "", data)
	require.NoError(s.t, err)
	s.blobs = append(s.blobs, blob{d: d, data: data, namespace: s.namespace})
	s.snapshot()
	return d
}
