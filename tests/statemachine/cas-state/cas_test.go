package casstate_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	sp "github.com/altdansalt/buildbuddy-validation/tests/statemachine/cas-state/surfaces"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type model struct {
	writes        map[string]int
	checkedWrites map[string]int
	blobs         map[string][]byte
	dirs          map[string]*repb.Directory
	roots         []*repb.Digest
	timestamps    map[string][2]int64
	zipDigest     *repb.Digest
	zipFiles      map[string][]byte
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
		var err error
		n, err = strconv.ParseInt(x, 10, 64)
		if err != nil {
			panic(err)
		}
		return n
	case float64:
		return int64(x)
	}
	panic(fmt.Sprintf("invalid numeric value %T: %v", v, v))
}

func TestBatchWriteState(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			secondKey := f.Web.CreateAPIKey(t, f.GroupID, "second-"+string(component), []string{"CACHE_WRITE"})
			c := harness.Dial(t, f.Endpoint(component).GRPC, secondKey)
			instance := "cas-state/" + string(component)
			m := &model{blobs: map[string][]byte{}, dirs: map[string]*repb.Directory{}}
			a := m.add([]byte("first file " + string(component)))
			m.zipFiles = map[string][]byte{"first.txt": []byte("zip first file"), "nested/second.txt": []byte("zip second file")}
			var zipBytes bytes.Buffer
			zw := zip.NewWriter(&zipBytes)
			for _, name := range []string{"first.txt", "nested/second.txt"} {
				w, err := zw.Create(name)
				require.NoError(t, err)
				_, err = w.Write(m.zipFiles[name])
				require.NoError(t, err)
			}
			require.NoError(t, zw.Close())
			m.zipDigest = m.add(zipBytes.Bytes())
			leaf := m.directory(t, &repb.Directory{Files: []*repb.FileNode{{Name: "a.txt", Digest: a}}})
			root := m.directory(t, &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "left", Digest: leaf}, {Name: "right", Digest: leaf}}})
			m.roots = append(m.roots, root)
			missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{a, leaf, root}, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			require.Len(t, missing.MissingBlobDigests, 3)
			uploaded := map[string]bool{}
			lastBatch := map[string]bool{}
			repeat := false
			writer := c
			write := func(compressed bool) {
				var requests []*repb.BatchUpdateBlobsRequest_Request
				if !repeat {
					lastBatch = map[string]bool{}
				}
				for k, b := range m.blobs {
					if (uploaded[k] && !repeat) || (repeat && !lastBatch[k]) {
						continue
					}
					uploaded[k] = true
					lastBatch[k] = true
					req := &repb.BatchUpdateBlobsRequest_Request{Digest: harness.Digest(b), Data: b}
					if compressed {
						var err error
						req.Data, err = harness.Compress(b)
						require.NoError(t, err)
						req.Compressor = repb.Compressor_ZSTD
					}
					requests = append(requests, req)
				}
				sort.Slice(requests, func(i, j int) bool { return key(requests[i].Digest) < key(requests[j].Digest) })
				res, err := writer.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: repb.DigestFunction_SHA256, Requests: requests})
				require.NoError(t, err)
				require.Len(t, res.Responses, len(requests))
				seen := map[string]bool{}
				for _, r := range res.Responses {
					require.Zero(t, r.GetStatus().GetCode())
					require.Contains(t, m.blobs, key(r.Digest))
					require.False(t, seen[key(r.Digest)])
					seen[key(r.Digest)] = true
					m.wrote(r.Digest)
				}
			}
			check := func() {
				t.Helper()
				start := time.Now()
				awaitModel(t, ctx, f, instance, m)
				t.Logf("cross-component convergence observed within %s", time.Since(start))
				checkState(t, ctx, f, c, component, instance, m)
				peer := harness.App
				if component == harness.App {
					peer = harness.CacheProxy
				}
				// Reuse the same independent model and instance on the other component.
				checkState(t, ctx, f, f.Clients(peer), peer, instance, m)
				time.Sleep(100 * time.Millisecond)
				checkState(t, ctx, f, c, component, instance, m)
				checkState(t, ctx, f, f.Clients(peer), peer, instance, m)
			}
			write(false)
			check()
			repeat = true
			write(false)
			check()
			repeat = false
			writer = f.Clients(harness.App)
			if component == harness.App {
				writer = f.Clients(harness.CacheProxy)
			}
			b := m.add([]byte("extended file " + string(component)))
			extended := m.directory(t, &repb.Directory{Files: []*repb.FileNode{{Name: "b.txt", Digest: b}}, Directories: []*repb.DirectoryNode{{Name: "previous", Digest: root}}})
			m.roots = append(m.roots, extended)
			write(false)
			check()
			repeat = true
			write(false)
			check()
			repeat = false
			writer = c
			z := m.add([]byte("compressed extension " + string(component)))
			compressedRoot := m.directory(t, &repb.Directory{Files: []*repb.FileNode{{Name: "z.txt", Digest: z}}, Directories: []*repb.DirectoryNode{{Name: "previous", Digest: extended}}})
			m.roots = append(m.roots, compressedRoot)
			write(true)
			check()
			repeat = true
			write(true)
			check()
			for _, compressed := range []bool{false, true} {
				b := []byte(fmt.Sprintf("stream extension %s %t", component, compressed))
				d := m.add(b)
				written, err := writer.Write(ctx, instance, b, compressed)
				require.NoError(t, err)
				require.True(t, proto.Equal(d, written))
				m.wrote(d)
				check()
			}
		})
	}
}

func checkState(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, component harness.Component, instance string, m *model) {
	t.Helper()
	var digests []*repb.Digest
	for _, b := range m.blobs {
		digests = append(digests, harness.Digest(b))
	}
	sort.Slice(digests, func(i, j int) bool { return key(digests[i]) < key(digests[j]) })
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
	compressedBatch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: digests, DigestFunction: repb.DigestFunction_SHA256, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
	require.NoError(t, err)
	require.Len(t, compressedBatch.Responses, len(digests))
	compressedSeen := map[string]bool{}
	for _, r := range compressedBatch.Responses {
		require.Zero(t, r.GetStatus().GetCode())
		require.Contains(t, m.blobs, key(r.Digest))
		require.False(t, compressedSeen[key(r.Digest)])
		compressedSeen[key(r.Digest)] = true
		data := r.Data
		if r.Compressor == repb.Compressor_ZSTD {
			data, err = harness.Decompress(data)
			require.NoError(t, err)
		} else {
			require.Equal(t, repb.Compressor_IDENTITY, r.Compressor)
		}
		require.Equal(t, m.blobs[key(r.Digest)], data)
	}
	for _, d := range digests {
		want := m.blobs[key(d)]
		data, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		require.Equal(t, want, data)
		require.Equal(t, want[1:len(want)-1], readStream(t, ctx, c, harness.ResourceName(instance, d, false), 1, int64(len(want)-2)))
		compressed, err := harness.Decompress(readStream(t, ctx, c, harness.ResourceName(instance, d, true), 0, 0))
		require.NoError(t, err)
		require.Equal(t, want, compressed)
		req := &sp.MetadataRequest{ResourceName: &sp.Resource{Digest: sd(d), InstanceName: instance, CacheType: 2, DigestFunction: 1}}
		meta := &sp.MetadataResponse{}
		invoke(t, ctx, c, "/cache.service.Cache/GetMetadata", req, meta)
		require.Equal(t, d.SizeBytes, meta.DigestSizeBytes)
		require.Greater(t, meta.StoredSizeBytes, int64(0))
		require.Greater(t, meta.LastModifyUsec, int64(0))
		require.LessOrEqual(t, meta.LastModifyUsec, time.Now().Add(time.Second).UnixMicro())
		if m.timestamps == nil {
			m.timestamps = map[string][2]int64{}
		}
		timestampKey := string(component) + "/" + key(d)
		if previous, ok := m.timestamps[timestampKey]; ok {
			require.GreaterOrEqual(t, meta.LastModifyUsec, previous[0])
			if m.checkedWrites[timestampKey] == m.writes[key(d)] {
				require.Equal(t, previous[0], meta.LastModifyUsec, "read-only/preserved digest modification time changed")
			}
			require.GreaterOrEqual(t, meta.LastAccessUsec, previous[1])
		}
		m.timestamps[timestampKey] = [2]int64{meta.LastModifyUsec, meta.LastAccessUsec}
		if m.checkedWrites == nil {
			m.checkedWrites = map[string]int{}
		}
		m.checkedWrites[timestampKey] = m.writes[key(d)]
		if component == harness.App {
			ui := &sp.MetadataResponse{}
			invoke(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", req, ui)
			require.Equal(t, meta.DigestSizeBytes, ui.DigestSizeBytes)
			require.Equal(t, meta.StoredSizeBytes, ui.StoredSizeBytes)
			require.Equal(t, meta.LastModifyUsec, ui.LastModifyUsec)
			resource := map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": "CAS", "digestFunction": "SHA256"}
			res, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": resource})
			require.NoError(t, err)
			require.Equal(t, d.SizeBytes, number(res["digestSizeBytes"]))
			require.Equal(t, meta.StoredSizeBytes, number(res["storedSizeBytes"]))
			require.Equal(t, meta.LastModifyUsec, number(res["lastModifyUsec"]))
			uri := "bytestream://localhost:1985/" + harness.ResourceName(instance, d, false)
			for _, path := range []string{"/file/download", "/file/view"} {
				req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+path+"?bytestream_url="+url.QueryEscape(uri)+"&filename=a.txt", nil)
				require.NoError(t, err)
				req.Header.Set("x-buildbuddy-api-key", f.APIKey)
				resp, err := f.Web.Client.Do(req)
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				require.NoError(t, err)
				require.Equal(t, 200, resp.StatusCode, "%s: %s", path, body)
				require.Equal(t, want, body, path)
			}
			compressedURI := "bytestream://localhost:1985/" + harness.ResourceName(instance, d, true)
			var compressedAPI []byte
			for _, part := range stream(t, ctx, c, "/api.v1.ApiService/GetFile", &sp.FileRequest{Uri: compressedURI}, func() proto.Message { return &sp.FileResponse{} }) {
				compressedAPI = append(compressedAPI, part.(*sp.FileResponse).Data...)
			}
			decoded, err := harness.Decompress(compressedAPI)
			require.NoError(t, err)
			require.Equal(t, want, decoded)
			decoded, err = harness.Decompress(httpProto(t, ctx, f, "/api/v1/GetFile", &sp.FileRequest{Uri: compressedURI}))
			require.NoError(t, err)
			require.Equal(t, want, decoded)
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
		if directory, ok := m.dirs[key(d)]; ok {
			cmd := exec.CommandContext(ctx, cli, "download", harness.ResourceName(instance, d, false), "--type=Directory", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
			out, err := cmd.Output()
			require.NoError(t, err)
			parsed := &repb.Directory{}
			require.NoError(t, protojson.Unmarshal(out, parsed))
			require.True(t, proto.Equal(directory, parsed), "CLI Directory JSON disagrees")
		}

	}
	if component == harness.App && m.zipDigest != nil {
		checkZip(t, ctx, f, c, instance, m)
	}
	for _, root := range m.roots {
		projection := map[string][2]int64{}
		m.projection(root, projection)
		var expected []string
		var walk func(*repb.Digest)
		walk = func(d *repb.Digest) {
			expected = append(expected, key(d))
			for _, child := range m.dirs[key(d)].Directories {
				walk(child.Digest)
			}
		}
		walk(root)
		sort.Strings(expected)
		checkDirectoryDownload(t, ctx, f, component, instance, m, root)
		s, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: root, PageSize: 1, DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		var dirs []*repb.Directory
		for {
			r, err := s.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			require.Empty(t, r.NextPageToken, "released server streams complete trees despite page-size hints")
			dirs = append(dirs, r.Directories...)
		}
		require.Equal(t, expected, treeKeys(t, dirs))
		if component != harness.App {
			continue
		}
		uiParts := stream(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetTree", &sp.TreeRequest{InstanceName: instance, RootDigest: sd(root), PageSize: 1, DigestFunction: 1}, func() proto.Message { return &sp.TreeResponse{} })
		dirs = nil
		for _, part := range uiParts {
			require.Empty(t, part.(*sp.TreeResponse).NextPageToken)
			for _, b := range part.(*sp.TreeResponse).Directories {
				d := &repb.Directory{}
				require.NoError(t, proto.Unmarshal(b, d))
				dirs = append(dirs, d)
			}
		}
		require.Equal(t, expected, treeKeys(t, dirs))
		sizes := &sp.SizesResponse{}
		invoke(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", &sp.SizesRequest{RootDigest: sd(root), InstanceName: instance, DigestFunction: 1}, sizes)
		got := map[string][2]int64{}
		for _, s := range sizes.Sizes {
			require.NotContains(t, got, s.Digest)
			got[s.Digest] = [2]int64{s.TotalSize, s.ChildCount}
		}
		require.Equal(t, hashProjection(projection), got)
		request := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "rootDigest": map[string]any{"hash": root.Hash, "sizeBytes": fmt.Sprint(root.SizeBytes)}, "instanceName": instance, "digestFunction": "SHA256"}
		res, err := f.Web.RPC("GetTreeDirectorySizes", request)
		require.NoError(t, err)
		got = map[string][2]int64{}
		for _, v := range res["sizes"].([]any) {
			s := v.(map[string]any)
			require.NotContains(t, got, s["digest"].(string))
			got[s["digest"].(string)] = [2]int64{number(s["totalSize"]), number(s["childCount"])}
		}
		require.Equal(t, hashProjection(projection), got)
		var jsonDirs []*repb.Directory
		for _, p := range httpStream(t, ctx, f, "/rpc/BuildBuddyService/GetTree", &sp.TreeRequest{InstanceName: instance, RootDigest: sd(root), PageSize: 1, DigestFunction: 1}, func() proto.Message { return &sp.TreeResponse{} }) {
			for _, b := range p.(*sp.TreeResponse).Directories {
				d := &repb.Directory{}
				require.NoError(t, proto.Unmarshal(b, d))
				jsonDirs = append(jsonDirs, d)
			}
		}

		require.Equal(t, expected, treeKeys(t, jsonDirs))
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

func readStream(t *testing.T, ctx context.Context, c *harness.Clients, rn string, offset, limit int64) []byte {
	t.Helper()
	s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: rn, ReadOffset: offset, ReadLimit: limit})
	require.NoError(t, err)
	var out []byte
	for {
		r, err := s.Recv()
		if err == io.EOF {
			return out
		}
		require.NoError(t, err)
		out = append(out, r.Data...)
	}
}

// Poll exact bytes, not merely existence. Full projections are checked after convergence.
func awaitModel(t *testing.T, ctx context.Context, f *harness.Fixture, instance string, m *model) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		ready := true
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			for _, b := range m.blobs {
				got, err := f.Clients(component).Read(pollCtx, instance, harness.Digest(b))
				if err != nil || !bytes.Equal(got, b) {
					ready = false
				}
			}
		}
		if ready {
			return
		}
		require.True(t, time.Now().Before(deadline), "exact-byte convergence exceeded 10s")
		time.Sleep(50 * time.Millisecond)
	}
}

func checkDirectoryDownload(t *testing.T, ctx context.Context, f *harness.Fixture, component harness.Component, instance string, m *model, root *repb.Digest) {
	t.Helper()
	dir := t.TempDir()
	cli, err := exec.LookPath("bb")
	require.NoError(t, err)
	cmd := exec.CommandContext(ctx, cli, "download", harness.ResourceName(instance, root, false), "--output_directory="+dir, "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	expected := map[string][]byte{}
	var walk func(*repb.Digest, string)
	walk = func(d *repb.Digest, prefix string) {
		for _, file := range m.dirs[key(d)].Files {
			expected[filepath.Join(prefix, file.Name)] = m.blobs[key(file.Digest)]
		}
		for _, child := range m.dirs[key(d)].Directories {
			walk(child.Digest, filepath.Join(prefix, child.Name))
		}
	}
	walk(root, "")
	got := map[string][]byte{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got[rel] = b
		return nil
	}))
	require.Equal(t, expected, got)
}

func checkZip(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, instance string, m *model) {
	t.Helper()
	uri := "bytestream://localhost:1985/" + harness.ResourceName(instance, m.zipDigest, false)
	res := &sp.ZipResponse{}
	invoke(t, ctx, c, "/buildbuddy.service.BuildBuddyService/GetZipManifest", &sp.ZipRequest{Uri: uri}, res)
	require.Len(t, res.GetManifest().GetEntry(), len(m.zipFiles))
	jsonRes, err := f.Web.RPC("GetZipManifest", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "uri": uri})
	require.NoError(t, err)
	entries := jsonRes["manifest"].(map[string]any)["entry"].([]any)
	require.Len(t, entries, len(m.zipFiles))
	seen := map[string]bool{}
	for _, value := range entries {
		entry := value.(map[string]any)
		name := entry["name"].(string)
		require.Contains(t, m.zipFiles, name)
		require.False(t, seen[name])
		seen[name] = true
		require.Equal(t, int64(len(m.zipFiles[name])), number(entry["uncompressedSize"]))
	}
	seen = map[string]bool{}
	for _, entry := range res.Manifest.Entry {
		require.Contains(t, m.zipFiles, entry.Name)
		require.False(t, seen[entry.Name])
		seen[entry.Name] = true
		require.Equal(t, int64(len(m.zipFiles[entry.Name])), entry.UncompressedSize)
		require.Equal(t, crc32.ChecksumIEEE(m.zipFiles[entry.Name]), entry.Crc32)
		b, err := proto.Marshal(entry)
		require.NoError(t, err)
		for _, path := range []string{"/file/download", "/file/view"} {
			req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+path+"?bytestream_url="+url.QueryEscape(uri)+"&z="+url.QueryEscape(base64.StdEncoding.EncodeToString(b)), nil)
			require.NoError(t, err)
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			resp, err := f.Web.Client.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, 200, resp.StatusCode, string(body))
			require.Equal(t, m.zipFiles[entry.Name], body)
		}
	}
}

// App-only deletion projections stay covered independently of the known proxy
// invalidation divergence. Every create/extension/delete/recreation step checks
// all remaining live bytes; deletion additionally checks absence projections.
func TestAppDeleteRecreate(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	second := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "lifecycle second", []string{"CACHE_WRITE"}))
	// App HTTP readiness can precede its internal self-gRPC client's reconnection.
	// Wait for that transport using a never-uploaded resource; this is fixture
	// readiness, not a retry that can mask a model mismatch.
	readinessDeadline := time.Now().Add(10 * time.Second)
	for {
		err := c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", &sp.MetadataRequest{ResourceName: &sp.Resource{Digest: sd(harness.Digest([]byte("readiness only"))), CacheType: 2, DigestFunction: 1}}, &sp.MetadataResponse{})
		if status.Code(err) == codes.NotFound {
			break
		}
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.True(t, time.Now().Before(readinessDeadline), "app self-gRPC readiness exceeded 10s")
		time.Sleep(50 * time.Millisecond)
	}
	instance := "app-lifecycle"
	m := &model{blobs: map[string][]byte{}, dirs: map[string]*repb.Directory{}}
	var original *repb.Digest
	var originalBytes []byte
	for step := 0; step < 3; step++ {
		b := []byte(fmt.Sprintf("app lifecycle immutable version %d", step))
		d := m.add(b)
		if step == 0 {
			original = d
			originalBytes = b
		}
		writer := c
		if step == 1 {
			writer = second
		}
		r, err := writer.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: repb.DigestFunction_SHA256, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: b}}})
		require.NoError(t, err)
		require.Len(t, r.Responses, 1)
		require.Zero(t, r.Responses[0].GetStatus().GetCode())
		m.wrote(d)
		checkState(t, ctx, f, c, harness.App, instance, m)
	}
	for repeat := 0; repeat < 2; repeat++ {
		invoke(t, ctx, second, "/api.v1.ApiService/DeleteFile", &sp.FileRequest{Uri: harness.ResourceName(instance, original, false)}, &sp.Empty{})
		delete(m.blobs, key(original))
		checkState(t, ctx, f, c, harness.App, instance, m)
		checkAbsentApp(t, ctx, f, c, instance, original)
		time.Sleep(100 * time.Millisecond)
		checkAbsentApp(t, ctx, f, second, instance, original)
	}
	written, err := second.Write(ctx, instance, originalBytes, true)
	require.NoError(t, err)
	require.True(t, proto.Equal(original, written))
	m.add(originalBytes)
	m.wrote(original)
	checkState(t, ctx, f, c, harness.App, instance, m)
	time.Sleep(100 * time.Millisecond)
	checkState(t, ctx, f, second, harness.App, instance, m)
}

func checkAbsentApp(t *testing.T, ctx context.Context, f *harness.Fixture, c *harness.Clients, instance string, d *repb.Digest) {
	t.Helper()
	missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Len(t, missing.MissingBlobDigests, 1)
	require.True(t, proto.Equal(d, missing.MissingBlobDigests[0]))
	for _, compressors := range [][]repb.Compressor_Value{nil, {repb.Compressor_ZSTD}} {
		r, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256, AcceptableCompressors: compressors})
		require.NoError(t, err)
		require.Len(t, r.Responses, 1)
		require.Equal(t, int32(codes.NotFound), r.Responses[0].GetStatus().GetCode())
		require.Empty(t, r.Responses[0].Data)
	}
	_, err = c.Read(ctx, instance, d)
	require.Equal(t, codes.NotFound, status.Code(err))
	for _, compressed := range []bool{false, true} {
		stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, compressed)})
		require.NoError(t, err)
		_, err = stream.Recv()
		require.Equal(t, codes.NotFound, status.Code(err))
	}

	metadataReq := &sp.MetadataRequest{ResourceName: &sp.Resource{Digest: sd(d), InstanceName: instance, CacheType: 2, DigestFunction: 1}}
	for _, method := range []string{"/cache.service.Cache/GetMetadata", "/buildbuddy.service.BuildBuddyService/GetCacheMetadata"} {
		err := c.Conn.Invoke(ctx, method, metadataReq, &sp.MetadataResponse{})
		require.Equal(t, codes.NotFound, status.Code(err), "%s: %v", method, err)
	}
	uri := "bytestream://localhost:1985/" + harness.ResourceName(instance, d, false)
	err = c.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", &sp.FileRequest{Uri: uri, End: d.SizeBytes}, &sp.FileResponse{})
	require.Equal(t, codes.NotFound, status.Code(err))
	s, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
	require.NoError(t, err)
	require.NoError(t, s.SendMsg(&sp.FileRequest{Uri: uri}))
	require.NoError(t, s.CloseSend())
	err = s.RecvMsg(&sp.FileResponse{})
	// Pooled bytestream client sanitizes all exhausted read attempts this way.
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "failed to read byte stream resource")
	resource := map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": "CAS", "digestFunction": "SHA256"}
	_, err = f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": resource})
	require.Error(t, err)
	require.Contains(t, err.Error(), "NotFound")
	for _, path := range []string{"/file/download", "/file/view"} {
		req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+path+"?bytestream_url="+url.QueryEscape(uri), nil)
		require.NoError(t, err)
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		resp, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		// Released handler falls back to invocation blobstore and requires invocation_id.
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
		require.Contains(t, string(body), "Missing invocation_id")
	}
	for _, path := range []string{"/api/v1/GetFile", "/api/v1/GetFileRange"} {
		b, err := proto.Marshal(&sp.FileRequest{Uri: uri, End: d.SizeBytes})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+path, bytes.NewReader(b))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/protobuf")
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		resp, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		if path == "/api/v1/GetFile" {
			require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
			require.Contains(t, string(body), "failed to read byte stream resource")
		} else {
			require.Equal(t, http.StatusInternalServerError, resp.StatusCode, string(body))
			require.Contains(t, string(body), "NotFound")
		}
	}
	cli, err := exec.LookPath("bb")
	require.NoError(t, err)
	cmd := exec.CommandContext(ctx, cli, "download", harness.ResourceName(instance, d, false), "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	body, err := cmd.CombinedOutput()
	require.Error(t, err, string(body))
	require.Contains(t, string(body), "NotFound")
}

func (m *model) wrote(d *repb.Digest) {
	if m.writes == nil {
		m.writes = map[string]int{}
	}
	m.writes[key(d)]++
}
