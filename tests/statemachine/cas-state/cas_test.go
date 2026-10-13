package casstate_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	sp "github.com/altdansalt/buildbuddy-validation/tests/statemachine/cas-state/surfaces"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type model struct {
	blobs map[string][]byte
	dirs  map[string]*repb.Directory
	roots []*repb.Digest
}

func key(d *repb.Digest) string            { return fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes) }
func sd(d *repb.Digest) *sp.Digest         { return &sp.Digest{Hash: d.Hash, SizeBytes: d.SizeBytes} }
func (m *model) add(b []byte) *repb.Digest { d := harness.Digest(b); m.blobs[key(d)] = b; return d }
func (m *model) directory(t *testing.T, d *repb.Directory) *repb.Digest {
	b, err := proto.Marshal(d)
	require.NoError(t, err)
	dg := m.add(b)
	m.dirs[key(dg)] = d
	return dg
}
func (m *model) projection(d *repb.Digest, out map[string][2]int64) (int64, int64) {
	dir := m.dirs[key(d)]
	size, count := d.SizeBytes, int64(0)
	for _, f := range dir.Files {
		size += f.Digest.SizeBytes
		count++
	}
	for _, child := range dir.Directories {
		s, c := m.projection(child.Digest, out)
		size += s
		count += 1 + c
	}
	out[key(d)] = [2]int64{size, count}
	return size, count
}
func treeKeys(t *testing.T, dirs []*repb.Directory) []string {
	var result []string
	for _, d := range dirs {
		b, err := proto.Marshal(d)
		require.NoError(t, err)
		result = append(result, key(harness.Digest(b)))
	}
	sort.Strings(result)
	return result
}
func invoke(t *testing.T, ctx context.Context, c *harness.Clients, method string, in, out proto.Message) {
	t.Helper()
	require.NoError(t, c.Conn.Invoke(ctx, method, in, out), method)
}
func stream(t *testing.T, ctx context.Context, c *harness.Clients, method string, in proto.Message, newResponse func() proto.Message) []proto.Message {
	t.Helper()
	s, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, method)
	require.NoError(t, err)
	require.NoError(t, s.SendMsg(in))
	require.NoError(t, s.CloseSend())
	var out []proto.Message
	for {
		r := newResponse()
		err = s.RecvMsg(r)
		if err == io.EOF {
			return out
		}
		require.NoError(t, err, method)
		out = append(out, r)
	}
}
func number(v any) int64 {
	switch x := v.(type) {
	case string:
		var n int64
		fmt.Sscan(x, &n)
		return n
	case float64:
		return int64(x)
	}
	return 0
}

func TestBatchWriteState(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			c := f.Clients(component)
			instance := "cas-state/" + string(component)
			m := &model{blobs: map[string][]byte{}, dirs: map[string]*repb.Directory{}}
			a := m.add([]byte("first file " + string(component)))
			leaf := m.directory(t, &repb.Directory{Files: []*repb.FileNode{{Name: "a.txt", Digest: a}}})
			root := m.directory(t, &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "left", Digest: leaf}, {Name: "right", Digest: leaf}}})
			m.roots = append(m.roots, root)
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{a, leaf, root}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, missing.MissingBlobDigests, 3)
			write := func(compressed bool) {
				var requests []*repb.BatchUpdateBlobsRequest_Request
				for _, b := range m.blobs {
					req := &repb.BatchUpdateBlobsRequest_Request{Digest: harness.Digest(b), Data: b}
					if compressed {
						var err error
						req.Data, err = harness.Compress(b)
						require.NoError(t, err)
						req.Compressor = repb.Compressor_ZSTD
					}
					requests = append(requests, req)
				}
				res, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: repb.DigestFunction_SHA256, Requests: requests})
				require.NoError(t, err)
				require.Len(t, res.Responses, len(requests))
				seen := map[string]bool{}
				for _, r := range res.Responses {
					require.Zero(t, r.GetStatus().GetCode())
					require.Contains(t, m.blobs, key(r.Digest))
					require.False(t, seen[key(r.Digest)])
					seen[key(r.Digest)] = true
				}
			}
			check := func() { t.Helper(); checkState(t, ctx, f, c, component, instance, m) }
			write(false)
			check()
			write(false)
			check()
			b := m.add([]byte("extended file " + string(component)))
			extended := m.directory(t, &repb.Directory{Files: []*repb.FileNode{{Name: "b.txt", Digest: b}}, Directories: []*repb.DirectoryNode{{Name: "previous", Digest: root}}})
			m.roots = append(m.roots, extended)
			write(false)
			check()
			write(false)
			check()
			z := m.add([]byte("compressed extension " + string(component)))
			compressedRoot := m.directory(t, &repb.Directory{Files: []*repb.FileNode{{Name: "z.txt", Digest: z}}, Directories: []*repb.DirectoryNode{{Name: "previous", Digest: extended}}})
			m.roots = append(m.roots, compressedRoot)
			write(true)
			check()
			write(true)
			check()
		})
	}
}

func checkState(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, component harness.Component, instance string, m *model) {
	t.Helper()
	var digests []*repb.Digest
	for _, b := range m.blobs {
		digests = append(digests, harness.Digest(b))
	}
	missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: digests, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Empty(t, missing.MissingBlobDigests)
	batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: digests, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Len(t, batch.Responses, len(digests))
	seen := map[string]bool{}
	for _, r := range batch.Responses {
		require.Zero(t, r.GetStatus().GetCode())
		require.Contains(t, m.blobs, key(r.Digest))
		require.False(t, seen[key(r.Digest)])
		seen[key(r.Digest)] = true
		require.Equal(t, m.blobs[key(r.Digest)], r.Data)
	}
	for _, d := range digests {
		want := m.blobs[key(d)]
		data, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		require.Equal(t, want, data)
		req := &sp.MetadataRequest{ResourceName: &sp.Resource{Digest: sd(d), InstanceName: instance, CacheType: 2, DigestFunction: 1}}
		meta := &sp.MetadataResponse{}
		invoke(t, ctx, c, "/cache.service.Cache/GetMetadata", req, meta)
		require.Equal(t, d.SizeBytes, meta.DigestSizeBytes)
		require.Greater(t, meta.StoredSizeBytes, int64(0))
		require.Greater(t, meta.LastModifyUsec, int64(0))
		if component == harness.App {
			ui := &sp.MetadataResponse{}
			invoke(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", req, ui)
			require.Equal(t, meta.DigestSizeBytes, ui.DigestSizeBytes)
			require.Equal(t, meta.StoredSizeBytes, ui.StoredSizeBytes)
			resource := map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": "CAS", "digestFunction": "SHA256"}
			res, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": resource})
			require.NoError(t, err)
			require.Equal(t, d.SizeBytes, number(res["digestSizeBytes"]))
			require.Equal(t, meta.StoredSizeBytes, number(res["storedSizeBytes"]))
			uri := "bytestream://localhost:1985/" + harness.ResourceName(instance, d, false)
			parts := stream(t, ctx, c, "/api.v1.ApiService/GetFile", &sp.FileRequest{Uri: uri}, func() proto.Message { return &sp.FileResponse{} })
			var got []byte
			for _, p := range parts {
				got = append(got, p.(*sp.FileResponse).Data...)
			}
			require.Equal(t, want, got)
			ranged := &sp.FileResponse{}
			start := int64(1)
			invoke(t, ctx, c, "/api.v1.ApiService/GetFileRange", &sp.FileRequest{Uri: uri, Start: start, End: d.SizeBytes}, ranged)
			require.Equal(t, want[start:], ranged.Data)
			require.Equal(t, want, httpProto(t, ctx, f, "/api/v1/GetFile", &sp.FileRequest{Uri: uri}))
			rangeHTTP := &sp.FileResponse{}
			require.NoError(t, proto.Unmarshal(httpProto(t, ctx, f, "/api/v1/GetFileRange", &sp.FileRequest{Uri: uri, Start: start, End: d.SizeBytes}), rangeHTTP))
			require.Equal(t, want[start:], rangeHTTP.Data)

		}
		// bb must be supplied by the hosted runner; never consult host login/key config.
		cli, err := exec.LookPath("bb")
		require.NoError(t, err, "runner needs released bb CLI")
		cmd := exec.CommandContext(ctx, cli, "download", harness.ResourceName(instance, d, false), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
		output, err := cmd.Output()
		require.NoError(t, err, "bb download %s", key(d))
		require.Equal(t, want, output)
	}
	for _, root := range m.roots {
		projection := map[string][2]int64{}
		m.projection(root, projection)
		var expected []string
		for k := range projection {
			expected = append(expected, k)
		}
		sort.Strings(expected)
		s, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: root, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		var dirs []*repb.Directory
		for {
			r, err := s.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			dirs = append(dirs, r.Directories...)
		}
		// Traversal may include a shared directory once or once per path. Compare its set.
		unique := func(keys []string) []string {
			set := map[string]bool{}
			for _, k := range keys {
				set[k] = true
			}
			var out []string
			for k := range set {
				out = append(out, k)
			}
			sort.Strings(out)
			return out
		}
		require.Equal(t, expected, unique(treeKeys(t, dirs)))
		if component != harness.App {
			continue
		}
		uiParts := stream(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetTree", &sp.TreeRequest{InstanceName: instance, RootDigest: sd(root), DigestFunction: 1}, func() proto.Message { return &sp.TreeResponse{} })
		dirs = nil
		for _, part := range uiParts {
			for _, b := range part.(*sp.TreeResponse).Directories {
				d := &repb.Directory{}
				require.NoError(t, proto.Unmarshal(b, d))
				dirs = append(dirs, d)
			}
		}
		require.Equal(t, expected, unique(treeKeys(t, dirs)))
		sizes := &sp.SizesResponse{}
		invoke(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", &sp.SizesRequest{RootDigest: sd(root), InstanceName: instance, DigestFunction: 1}, sizes)
		got := map[string][2]int64{}
		for _, s := range sizes.Sizes {
			got[s.Digest] = [2]int64{s.TotalSize, s.ChildCount}
		}
		require.Equal(t, hashProjection(projection), got)
		request := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "rootDigest": map[string]any{"hash": root.Hash, "sizeBytes": fmt.Sprint(root.SizeBytes)}, "instanceName": instance, "digestFunction": "SHA256"}
		res, err := f.Web.RPC("GetTreeDirectorySizes", request)
		require.NoError(t, err)
		got = map[string][2]int64{}
		for _, v := range res["sizes"].([]any) {
			s := v.(map[string]any)
			got[s["digest"].(string)] = [2]int64{number(s["totalSize"]), number(s["childCount"])}
		}
		require.Equal(t, hashProjection(projection), got)
		var jsonDirs []*repb.Directory
		for _, p := range httpStream(t, ctx, f, "/rpc/BuildBuddyService/GetTree", &sp.TreeRequest{InstanceName: instance, RootDigest: sd(root), DigestFunction: 1}, func() proto.Message { return &sp.TreeResponse{} }) {
			for _, b := range p.(*sp.TreeResponse).Directories {
				d := &repb.Directory{}
				require.NoError(t, proto.Unmarshal(b, d))
				jsonDirs = append(jsonDirs, d)
			}
		}

		require.Equal(t, expected, unique(treeKeys(t, jsonDirs)))
	}
}

// HTTP transport used by the web UI for server-streaming RPCs.
func httpStream(t *testing.T, ctx context.Context, f *harness.Fixture, path string, in proto.Message, newResponse func() proto.Message) []proto.Message {
	t.Helper()
	b, err := proto.Marshal(in)
	require.NoError(t, err)
	frame := make([]byte, 5)
	binary.BigEndian.PutUint32(frame[1:], uint32(len(b)))
	frame = append(frame, b...)
	req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+path, bytes.NewReader(frame))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/proto+prefixed")
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	resp, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, string(body))
	var out []proto.Message
	status := resp.Header.Get("grpc-status")
	for len(body) > 0 {
		require.GreaterOrEqual(t, len(body), 5)
		flags := body[0]
		n := int(binary.BigEndian.Uint32(body[1:5]))
		body = body[5:]
		require.GreaterOrEqual(t, len(body), n)
		payload := body[:n]
		body = body[n:]
		if flags == 128 {
			require.Contains(t, strings.ToLower(string(payload)), "grpc-status: 0")
			status = "0"
			continue
		}
		require.Zero(t, flags)
		r := newResponse()
		require.NoError(t, proto.Unmarshal(payload, r))
		out = append(out, r)
	}
	require.Equal(t, "0", status)
	return out
}

// v2.314.0 directory-size responses use the hash alone as the key.
func hashProjection(in map[string][2]int64) map[string][2]int64 {
	out := map[string][2]int64{}
	for k, v := range in {
		out[strings.Split(k, "/")[0]] = v
	}
	return out
}

func httpProto(t *testing.T, ctx context.Context, f *harness.Fixture, path string, in proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(in)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+path, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	resp, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, string(b))
	return b
}
