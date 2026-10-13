package convergence_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func wire(b []byte) *emptypb.Empty { m := &emptypb.Empty{}; m.ProtoReflect().SetUnknown(b); return m }
func dataField(t *testing.T, m *emptypb.Empty, fieldNum protowire.Number) []byte {
	t.Helper()
	b := m.ProtoReflect().GetUnknown()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			require.GreaterOrEqual(t, k, 0)
			if n == fieldNum {
				return v
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			require.Greater(t, k, 0)
			b = b[k:]
		}
	}
	return nil
}
func varint(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}

// Every uploaded object, including Action/Command/Directory, is checked before
// the next mutation. Empty blobs have no valid nonempty range.
func checkBlobSurfacesOnce(t *testing.T, ctx context.Context, f *harness.Fixture, clients []*harness.Clients, d *repb.Digest, expected []byte) {
	t.Helper()
	started := time.Now()
	for side, c := range clients {
		compressedSize := 0
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		require.Empty(t, missing.MissingBlobDigests)
		for _, compressed := range []bool{false, true} {
			batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, batch.Responses, 1)
			r := batch.Responses[0]
			require.True(t, proto.Equal(d, r.Digest))
			require.Zero(t, r.GetStatus().GetCode())
			b := r.Data
			if r.Compressor == repb.Compressor_ZSTD {
				b, err = harness.Decompress(b)
				require.NoError(t, err)
			} else {
				require.Equal(t, repb.Compressor_IDENTITY, r.Compressor)
			}
			require.Equal(t, expected, b)
			stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName("", d, compressed)})
			require.NoError(t, err)
			var out []byte
			for {
				r, err := stream.Recv()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				out = append(out, r.Data...)
			}
			if compressed {
				compressedSize = len(out)
				out, err = harness.Decompress(out)
				require.NoError(t, err)
			}
			require.Equal(t, expected, out, "side %d", side)
		}
		if len(expected) > 1 {
			start, end := int64(1), int64(len(expected))
			stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName("", d, false), ReadOffset: start, ReadLimit: end - start})
			require.NoError(t, err)
			var out []byte
			for {
				r, err := stream.Recv()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				out = append(out, r.Data...)
			}
			require.Equal(t, expected[start:end], out)
		}
		md, err := metadata(ctx, c, d, 2, "/cache.service.Cache/GetMetadata")
		require.NoError(t, err)
		require.Equal(t, uint64(d.SizeBytes), md[5])
		require.Greater(t, md[4], uint64(0))
		require.Greater(t, md[3], uint64(0))
		require.True(t, md[2] == uint64(len(expected)) || md[2] == uint64(compressedSize), "stored size %d does not match identity %d or encoded zstd %d", md[2], len(expected), compressedSize)
		if d.SizeBytes > 0 {
			require.Greater(t, md[2], uint64(0))
		}
	}
	for _, compressed := range []bool{false, true} {
		uri := "bytestream://app:1985/" + harness.ResourceName("", d, compressed)
		req := wire(field(nil, 1, []byte(uri)))
		stream, err := clients[0].Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
		require.NoError(t, err)
		require.NoError(t, stream.SendMsg(req))
		require.NoError(t, stream.CloseSend())
		var out []byte
		for {
			rsp := &emptypb.Empty{}
			err := stream.RecvMsg(rsp)
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			out = append(out, dataField(t, rsp, 1)...)
		}
		if compressed {
			out, err = harness.Decompress(out)
			require.NoError(t, err)
		}
		require.Equal(t, expected, out)
		for _, endpoint := range []string{"/api/v1/GetFile"} {
			payload, _ := json.Marshal(map[string]any{"uri": uri})
			req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+endpoint, bytes.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			resp, err := f.Web.Client.Do(req)
			require.NoError(t, err)
			out, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, 200, resp.StatusCode, "%s", out)
			if compressed {
				out, err = harness.Decompress(out)
				require.NoError(t, err)
			}
			require.Equal(t, expected, out)
		}
	}
	if len(expected) > 1 {
		uri := "bytestream://app:1985/" + harness.ResourceName("", d, false)
		req := wire(varint(varint(field(nil, 1, []byte(uri)), 2, 1), 3, uint64(len(expected))))
		rsp := &emptypb.Empty{}
		require.NoError(t, clients[0].Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", req, rsp))
		require.Equal(t, expected[1:], dataField(t, rsp, 1))
		payload, _ := json.Marshal(map[string]any{"uri": uri, "start": "1", "end": strconv.Itoa(len(expected))})
		request, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/GetFileRange", bytes.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("x-buildbuddy-api-key", f.APIKey)
		resp, err := f.Web.Client.Do(request)
		require.NoError(t, err)
		defer resp.Body.Close()
		var result map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		require.Equal(t, 200, resp.StatusCode, "%v", result)
		decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(result["data"]))
		require.NoError(t, err)
		require.Equal(t, expected[1:], decoded)
	}
	md, err := metadata(ctx, clients[0], d, 2, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata")
	require.NoError(t, err)
	ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "cacheType": 2, "digestFunction": 1}})
	require.NoError(t, err)
	for n, name := range map[int]string{2: "storedSizeBytes", 4: "lastModifyUsec", 5: "digestSizeBytes"} {
		require.Equal(t, strconv.FormatUint(md[n], 10), fmt.Sprint(ui[name]), name)
	}
	if v, ok := ui["lastAccessUsec"]; ok {
		access, err := strconv.ParseUint(fmt.Sprint(v), 10, 64)
		require.NoError(t, err)
		require.GreaterOrEqual(t, access, md[3])
	}
	m := model{blobs: map[string][]byte{d.Hash: expected}, digests: []*repb.Digest{d}}
	require.NoError(t, checkHTTP(ctx, f, nil, m))
	for side := 0; side < 2; side++ {
		cmd := exec.CommandContext(ctx, "bb", "download", "--target=grpc://"+f.Endpoint([]harness.Component{harness.App, harness.CacheProxy}[side]).GRPC, "--api_key="+f.APIKey, d.Hash+"/"+strconv.FormatInt(d.SizeBytes, 10))
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
		out, err := cmd.Output()
		require.NoError(t, err)
		require.Equal(t, expected, out)
	}
	t.Logf("blob %s all surfaces observed in %s", d.Hash, time.Since(started))
}

func checkRecursiveCLI(t *testing.T, ctx context.Context, f *harness.Fixture, d *repb.Digest, expected []byte) {
	t.Helper()
	for _, side := range []harness.Component{harness.App, harness.CacheProxy} {
		dir := t.TempDir()
		cmd := exec.CommandContext(ctx, "bb", "download", "--target=grpc://"+f.Endpoint(side).GRPC, "--api_key="+f.APIKey, "--output_directory="+dir, d.Hash+"/"+strconv.FormatInt(d.SizeBytes, 10))
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
		out, err := cmd.Output()
		require.NoError(t, err, "%s", out)
		parsed := &repb.Directory{}
		require.NoError(t, protojson.Unmarshal(out, parsed))
		child := &repb.Directory{Files: []*repb.FileNode{{Name: "output", Digest: harness.Digest(expected)}}}
		_, childDigest, err := harness.MarshalDigest(child)
		require.NoError(t, err)
		require.True(t, proto.Equal(&repb.Directory{Directories: []*repb.DirectoryNode{{Name: "subdir", Digest: childDigest}}}, parsed))
		b, err := os.ReadFile(filepath.Join(dir, "subdir", "output"))
		require.NoError(t, err)
		require.Equal(t, expected, b)
	}
}

func uploadCheckedProto(t *testing.T, ctx context.Context, f *harness.Fixture, clients []*harness.Clients, msg proto.Message) (*repb.Digest, error) {
	b, d, err := harness.MarshalDigest(msg)
	if err != nil {
		return nil, err
	}
	got, err := clients[0].Upload(ctx, "", b)
	if err != nil {
		return nil, err
	}
	require.True(t, proto.Equal(d, got))
	checkBlobSurfaces(t, ctx, f, clients, d, b)
	if dir, ok := msg.(*repb.Directory); ok {
		checkUITree(t, ctx, f, clients[0], d, dir)
		checkDirectorySizesGRPC(t, ctx, clients[0], d, map[string][2]uint64{d.Hash: {uint64(d.SizeBytes), 0}})
		for _, c := range clients {
			stream, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{RootDigest: d, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			rsp, err := stream.Recv()
			require.NoError(t, err)
			require.Len(t, rsp.Directories, 1)
			require.True(t, proto.Equal(dir, rsp.Directories[0]))
			_, err = stream.Recv()
			require.Equal(t, io.EOF, err)
		}
	}
	return d, nil
}
func checkUITree(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, d *repb.Digest, dirs ...*repb.Directory) {
	db, err := proto.Marshal(d)
	require.NoError(t, err)
	req := wire(varint(field(nil, 3, db), 6, 1))
	rsp := &emptypb.Empty{}
	stream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree")
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(req))
	require.NoError(t, stream.CloseSend())
	var frames []byte
	for {
		frame := &emptypb.Empty{}
		err := stream.RecvMsg(frame)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		frames = append(frames, frame.ProtoReflect().GetUnknown()...)
	}
	rsp.ProtoReflect().SetUnknown(frames)
	expected := map[string]bool{}
	for _, dir := range dirs {
		_, digest, err := harness.MarshalDigest(dir)
		require.NoError(t, err)
		expected[digest.Hash] = true
	}
	b := rsp.ProtoReflect().GetUnknown()
	seen := map[string]bool{}
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			require.GreaterOrEqual(t, k, 0)
			b = b[k:]
			if n == 2 {
				dir := &repb.Directory{}
				require.NoError(t, proto.Unmarshal(v, dir))
				_, digest, err := harness.MarshalDigest(dir)
				require.NoError(t, err)
				require.True(t, expected[digest.Hash])
				require.False(t, seen[digest.Hash])
				seen[digest.Hash] = true
			}
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			require.Greater(t, k, 0)
			b = b[k:]
		}
	}
	require.Equal(t, expected, seen)
	body := make([]byte, 5)
	binary.BigEndian.PutUint32(body[1:], uint32(len(req.ProtoReflect().GetUnknown())))
	body = append(body, req.ProtoReflect().GetUnknown()...)
	request, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/GetTree", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/proto+prefixed")
	request.Header.Set("x-buildbuddy-api-key", f.APIKey)
	resp, err := f.Web.Client.Do(request)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	seen = map[string]bool{}
	statusSeen := false
	for {
		var header [5]byte
		_, err := io.ReadFull(resp.Body, header[:])
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		payload := make([]byte, binary.BigEndian.Uint32(header[1:]))
		_, err = io.ReadFull(resp.Body, payload)
		require.NoError(t, err)
		if header[0] == 0x80 {
			require.Contains(t, strings.ToLower(string(payload)), "grpc-status: 0")
			statusSeen = true
			continue
		}
		require.Zero(t, header[0])
		// BuildBuddy GetTree fields differ from REAPI by the context field.
		b := payload
		for len(b) > 0 {
			n, typ, k := protowire.ConsumeTag(b)
			require.Greater(t, k, 0)
			b = b[k:]
			if typ == protowire.BytesType {
				v, k := protowire.ConsumeBytes(b)
				require.GreaterOrEqual(t, k, 0)
				b = b[k:]
				if n == 2 {
					dir := &repb.Directory{}
					require.NoError(t, proto.Unmarshal(v, dir))
					_, digest, err := harness.MarshalDigest(dir)
					require.NoError(t, err)
					require.True(t, expected[digest.Hash])
					require.False(t, seen[digest.Hash])
					seen[digest.Hash] = true
				}
			} else {
				k := protowire.ConsumeFieldValue(n, typ, b)
				require.Greater(t, k, 0)
				b = b[k:]
			}
		}
	}
	require.True(t, statusSeen, "missing HTTP stream status")
	require.Equal(t, expected, seen)
}

func checkDirectorySizesGRPC(t *testing.T, ctx context.Context, c *harness.Clients, d *repb.Digest, expected map[string][2]uint64) {
	db, err := proto.Marshal(d)
	require.NoError(t, err)
	req := wire(varint(field(nil, 2, db), 4, 1))
	rsp := &emptypb.Empty{}
	require.NoError(t, c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", req, rsp))
	seen := map[string][2]uint64{}
	b := rsp.ProtoReflect().GetUnknown()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			require.GreaterOrEqual(t, k, 0)
			b = b[k:]
			if n == 2 {
				item := wire(v)
				hash := string(dataField(t, item, 1))
				numbers := map[int]uint64{}
				for len(v) > 0 {
					n, typ, k := protowire.ConsumeTag(v)
					require.Greater(t, k, 0)
					v = v[k:]
					if typ == protowire.VarintType {
						value, k := protowire.ConsumeVarint(v)
						require.Greater(t, k, 0)
						numbers[int(n)] = value
						v = v[k:]
					} else {
						k := protowire.ConsumeFieldValue(n, typ, v)
						require.Greater(t, k, 0)
						v = v[k:]
					}
				}
				_, duplicate := seen[hash]
				require.False(t, duplicate)
				seen[hash] = [2]uint64{numbers[2], numbers[3]}
			}
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			require.Greater(t, k, 0)
			b = b[k:]
		}
	}
	require.Equal(t, expected, seen)
}

// Completed writes are synchronous. Measure the first full read sweep, then
// require every surface to remain correct across spaced sweeps. The measured
// duration is an observation upper bound, not a claim of propagation latency.
func checkBlobSurfaces(t *testing.T, ctx context.Context, f *harness.Fixture, clients []*harness.Clients, d *repb.Digest, expected []byte) {
	t.Helper()
	checkBlobSurfacesOnce(t, ctx, f, clients, d, expected)
	for stable := 0; stable < 2; stable++ {
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatal(ctx.Err())
		case <-timer.C:
		}
		checkBlobSurfacesOnce(t, ctx, f, clients, d, expected)
	}
}

func checkACMetadataPair(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, d *repb.Digest) {
	t.Helper()
	md, err := metadata(ctx, c, d, 1, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata")
	require.NoError(t, err)
	require.Greater(t, md[2], uint64(0))
	require.Greater(t, md[3], uint64(0))
	require.Greater(t, md[4], uint64(0))
	require.Equal(t, uint64(d.SizeBytes), md[5])
	direct, err := metadata(ctx, c, d, 1, "/cache.service.Cache/GetMetadata")
	require.NoError(t, err)
	for _, n := range []int{2, 4, 5} {
		require.Equal(t, md[n], direct[n])
	}
	require.GreaterOrEqual(t, direct[3], md[3])
	ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "cacheType": 1, "digestFunction": 1}})
	require.NoError(t, err)
	for n, name := range map[int]string{2: "storedSizeBytes", 4: "lastModifyUsec", 5: "digestSizeBytes"} {
		require.Equal(t, strconv.FormatUint(md[n], 10), fmt.Sprint(ui[name]))
	}
	atime, err := strconv.ParseUint(fmt.Sprint(ui["lastAccessUsec"]), 10, 64)
	require.NoError(t, err)
	require.GreaterOrEqual(t, atime, md[3])
}

func readActionAPI(ctx context.Context, c *harness.Clients, d *repb.Digest) (*repb.ActionResult, error) {
	uri := fmt.Sprintf("actioncache://app:1985/blobs/ac/%s/%d", d.Hash, d.SizeBytes)
	stream, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
	if err != nil {
		return nil, err
	}
	if err := stream.SendMsg(wire(field(nil, 1, []byte(uri)))); err != nil {
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	var data []byte
	for {
		rsp := &emptypb.Empty{}
		err := stream.RecvMsg(rsp)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		b := rsp.ProtoReflect().GetUnknown()
		for len(b) > 0 {
			n, typ, k := protowire.ConsumeTag(b)
			if k < 0 {
				return nil, fmt.Errorf("invalid API frame tag")
			}
			b = b[k:]
			if typ == protowire.BytesType {
				v, k := protowire.ConsumeBytes(b)
				if k < 0 {
					return nil, fmt.Errorf("invalid API frame payload")
				}
				if n == 1 {
					data = append(data, v...)
				}
				b = b[k:]
			} else {
				k := protowire.ConsumeFieldValue(n, typ, b)
				if k < 0 {
					return nil, fmt.Errorf("invalid API frame field")
				}
				b = b[k:]
			}
		}
	}
	got := &repb.ActionResult{}
	if err := proto.Unmarshal(data, got); err != nil {
		return nil, err
	}
	return got, nil
}
