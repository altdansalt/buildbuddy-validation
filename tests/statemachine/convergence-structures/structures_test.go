package structures_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	"google.golang.org/protobuf/proto"
)

const instance = ""

// The UI messages are wire-compatible subsets of proto/cache.proto. Keeping
// these tiny wire adapters here avoids adding BuildBuddy source dependencies.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func field(b []byte, n protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, n, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}
func fields(b []byte, n protowire.Number) ([][]byte, error) {
	var out [][]byte
	for len(b) > 0 {
		tag, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			return nil, fmt.Errorf("bad tag")
		}
		b = b[k:]
		k = protowire.ConsumeFieldValue(tag, typ, b)
		if k < 0 {
			return nil, fmt.Errorf("bad field")
		}
		if tag == n && typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(b)
			out = append(out, v)
		}
		b = b[k:]
	}
	return out, nil
}
func key(d *repb.Digest) string { return fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes) }

type model struct {
	blobs    map[string][]byte
	dirs     map[string]*repb.Directory
	roots    []*repb.Digest
	mappings map[string][]*repb.Digest
}

func (m *model) put(t *testing.T, ctx context.Context, c *harness.Clients, b []byte, write bool) *repb.Digest {
	var d *repb.Digest
	var err error
	if write {
		d, err = c.Write(ctx, instance, b, false)
	} else {
		d, err = c.Upload(ctx, instance, b)
	}
	require.NoError(t, err)
	m.blobs[key(d)] = b
	return d
}
func (m *model) directory(t *testing.T, ctx context.Context, c *harness.Clients, dir *repb.Directory, write bool) *repb.Digest {
	b, _, err := harness.MarshalDigest(dir)
	require.NoError(t, err)
	d := m.put(t, ctx, c, b, write)
	if _, ok := m.dirs[key(d)]; !ok {
		m.roots = append(m.roots, d)
	}
	m.dirs[key(d)] = dir
	return d
}
func (m *model) reachable(d *repb.Digest, out map[string]*repb.Directory) {
	if _, ok := out[key(d)]; ok {
		return
	}
	dir := m.dirs[key(d)]
	out[key(d)] = dir
	for _, child := range dir.Directories {
		m.reachable(child.Digest, out)
	}
}
func (m *model) size(d *repb.Digest) (int64, int64) {
	dir := m.dirs[key(d)]
	size, count := d.SizeBytes, int64(len(dir.Files)+len(dir.Directories))
	for _, f := range dir.Files {
		size += f.Digest.SizeBytes
	}
	for _, sub := range dir.Directories {
		s, c := m.size(sub.Digest)
		size += s
		count += c
	}
	return size, count
}
func tree(ctx context.Context, c *harness.Clients, d *repb.Digest, ui bool) (map[string]*repb.Directory, error) {
	out := map[string]*repb.Directory{}
	add := func(dir *repb.Directory) { addDirectory(out, dir) }
	if !ui {
		token := ""
		for {
			s, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: d, PageSize: 1, PageToken: token, DigestFunction: repb.DigestFunction_SHA256})
			if err != nil {
				return nil, err
			}
			token = ""
			for {
				r, err := s.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, err
				}
				for _, dir := range r.Directories {
					add(dir)
				}
				token = r.NextPageToken
			}
			if token == "" {
				break
			}
		}
	} else {
		dg, _ := proto.Marshal(d)
		req := field(field(nil, 2, []byte(instance)), 3, dg)
		s, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree", grpc.ForceCodec(wireCodec{}))
		if err != nil {
			return nil, err
		}
		if err = s.SendMsg(req); err != nil {
			return nil, err
		}
		if err = s.CloseSend(); err != nil {
			return nil, err
		}
		for {
			var b []byte
			err = s.RecvMsg(&b)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			ds, err := fields(b, 2)
			if err != nil {
				return nil, err
			}
			for _, v := range ds {
				dir := &repb.Directory{}
				if err = proto.Unmarshal(v, dir); err != nil {
					return nil, err
				}
				add(dir)
			}
		}
	}
	return out, nil
}
func (m *model) check(ctx context.Context, f *harness.Fixture, clients []*harness.Clients) error {
	if err := m.extraSurfaces(ctx, f, clients); err != nil {
		return err
	}
	for _, c := range clients {
		digests := []*repb.Digest{}
		for k, b := range m.blobs {
			d := harness.Digest(b)
			if key(d) != k {
				return fmt.Errorf("model digest")
			}
			digests = append(digests, d)
			got, err := c.Read(ctx, instance, d)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, b) {
				return fmt.Errorf("ByteStream mismatch %s", k)
			}
		}
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: digests, DigestFunction: repb.DigestFunction_SHA256})
		if err != nil {
			return err
		}
		if len(missing.MissingBlobDigests) != 0 {
			return fmt.Errorf("acknowledged children missing: %v", missing)
		}
		// Respect the advertised 4MB batch limit: each modeled blob is <4MB.
		for _, dg := range digests {
			batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{dg}, DigestFunction: repb.DigestFunction_SHA256})
			if err != nil {
				return fmt.Errorf("BatchRead: %w", err)
			}
			if len(batch.Responses) != 1 {
				return fmt.Errorf("batch count")
			}
			r := batch.Responses[0]
			if r.GetStatus().GetCode() != 0 || !proto.Equal(r.Digest, dg) || !bytes.Equal(r.Data, m.blobs[key(dg)]) {
				return fmt.Errorf("BatchRead mismatch %s: %v", key(dg), r.Status)
			}
		}

		for _, root := range m.roots {
			want := map[string]*repb.Directory{}
			m.expectedTree(root, want)
			got, err := tree(ctx, c, root, false)
			if err != nil {
				return err
			}
			if len(got) != len(want) {
				return fmt.Errorf("tree count %d != %d", len(got), len(want))
			}
			for k, d := range want {
				if !proto.Equal(d, got[k]) {
					return fmt.Errorf("tree mismatch %s", k)
				}
			}
		}
		for k, chunks := range m.mappings {
			d := harness.Digest(m.blobs[k])
			r, err := c.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{InstanceName: instance, BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
			if err != nil {
				return err
			}
			if len(r.ChunkDigests) != len(chunks) {
				return fmt.Errorf("chunk count")
			}
			var reconstructed []byte
			for i, ch := range r.ChunkDigests {
				if !proto.Equal(ch, chunks[i]) {
					return fmt.Errorf("chunk order mismatch")
				}
				b, err := c.Read(ctx, instance, ch)
				if err != nil {
					return err
				}
				reconstructed = append(reconstructed, b...)
			}
			if !bytes.Equal(reconstructed, m.blobs[k]) {
				return fmt.Errorf("reconstruction mismatch")
			}
		}
	}
	for _, root := range m.roots {
		want := map[string]*repb.Directory{}
		m.expectedTree(root, want)
		got, err := tree(ctx, clients[0], root, true)
		if err != nil {
			return fmt.Errorf("UI gRPC GetTree: %w", err)
		}
		if len(got) != len(want) {
			return fmt.Errorf("UI tree count")
		}
		for k, d := range want {
			if !proto.Equal(d, got[k]) {
				return fmt.Errorf("UI tree mismatch")
			}
		}
		httpGot, err := httpTree(ctx, f, root)
		if err != nil {
			return err
		}
		if len(httpGot) != len(want) {
			return fmt.Errorf("HTTP tree count")
		}
		for k, d := range want {
			if !proto.Equal(d, httpGot[k]) {
				return fmt.Errorf("HTTP tree mismatch")
			}
		}
		want = map[string]*repb.Directory{}
		m.reachable(root, want)
		req := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "instanceName": instance, "rootDigest": map[string]any{"hash": root.Hash, "sizeBytes": strconv.FormatInt(root.SizeBytes, 10)}, "digestFunction": "SHA256"}
		res, err := f.Web.RPC("GetTreeDirectorySizes", req)
		if err != nil {
			return err
		}
		entries, ok := res["sizes"].([]any)
		if !ok || len(entries) != len(want) {
			return fmt.Errorf("HTTP size membership count")
		}
		sizes := map[string][2]int64{}
		for _, entry := range entries {
			v := entry.(map[string]any)
			parse := func(x any) int64 {
				n, err := strconv.ParseInt(fmt.Sprint(x), 10, 64)
				if err != nil {
					return -1
				}
				return n
			}
			if _, ok := sizes[v["digest"].(string)]; ok {
				return fmt.Errorf("duplicate HTTP size")
			}
			sizes[v["digest"].(string)] = [2]int64{parse(v["totalSize"]), parse(v["childCount"])}
		}
		for k := range want {
			d := harness.Digest(m.blobs[k])
			s, c := m.size(d)
			if sizes[d.Hash] != [2]int64{s, c} {
				return fmt.Errorf("UI size %s: %v want %v", k, sizes[d.Hash], [2]int64{s, c})
			}
		}
		// Unary gRPC counterpart of the HTTP UI endpoint must return the same bytes.
		dg, _ := proto.Marshal(root)
		var raw []byte
		err = clients[0].Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", field(field(nil, 2, dg), 3, []byte(instance)), &raw, grpc.ForceCodec(wireCodec{}))
		if err != nil {
			return err
		}
		ss, err := fields(raw, 2)
		if err != nil {
			return err
		}
		if len(ss) != len(entries) {
			return fmt.Errorf("UI grpc size count")
		}
		seenSizes := map[string]bool{}
		for _, v := range ss {
			var digest string
			var total, count int64
			for len(v) > 0 {
				n, typ, k := protowire.ConsumeTag(v)
				if k < 0 {
					return fmt.Errorf("bad size tag")
				}
				v = v[k:]
				k = protowire.ConsumeFieldValue(n, typ, v)
				if k < 0 {
					return fmt.Errorf("bad size value")
				}
				if n == 1 {
					b, _ := protowire.ConsumeBytes(v)
					digest = string(b)
				}
				if n == 2 || n == 3 {
					x, _ := protowire.ConsumeVarint(v)
					if n == 2 {
						total = int64(x)
					} else {
						count = int64(x)
					}
				}
				v = v[k:]
			}
			if seenSizes[digest] {
				return fmt.Errorf("duplicate grpc size")
			}
			seenSizes[digest] = true
			if _, ok := sizes[digest]; !ok {
				return fmt.Errorf("unknown grpc size")
			}
			if sizes[digest] != [2]int64{total, count} {
				return fmt.Errorf("grpc/http size disagreement")
			}
		}

	}
	return nil
}

// The web UI uses protolet's framed HTTP transport for streaming GetTree.
func httpTree(ctx context.Context, f *harness.Fixture, d *repb.Digest) (map[string]*repb.Directory, error) {
	dg, _ := proto.Marshal(d)
	rc := field(nil, 2, []byte(f.GroupID))
	req := field(field(field(nil, 1, rc), 2, []byte(instance)), 3, dg)
	framed := make([]byte, 5)
	binary.BigEndian.PutUint32(framed[1:], uint32(len(req)))
	framed = append(framed, req...)
	r, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/GetTree", bytes.NewReader(framed))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/proto+prefixed")
	rsp, err := f.Web.Client.Do(r)
	if err != nil {
		return nil, err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP tree status %d", rsp.StatusCode)
	}
	out := map[string]*repb.Directory{}
	statusSeen := rsp.Header.Get("grpc-status") == "0"
	for {
		var header [5]byte
		_, err := io.ReadFull(rsp.Body, header[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(header[1:])
		if n > 64<<20 {
			return nil, fmt.Errorf("oversized HTTP frame")
		}
		body := make([]byte, n)
		if _, err = io.ReadFull(rsp.Body, body); err != nil {
			return nil, err
		}
		if header[0] == 128 {
			if !strings.Contains(strings.ToLower(string(body)), "grpc-status: 0") {
				return nil, fmt.Errorf("HTTP tree trailers %s", body)
			}
			statusSeen = true
			continue
		}
		if header[0] != 0 {
			return nil, fmt.Errorf("unexpected frame")
		}
		ds, err := fields(body, 2)
		if err != nil {
			return nil, err
		}
		for _, v := range ds {
			dir := &repb.Directory{}
			if err := proto.Unmarshal(v, dir); err != nil {
				return nil, err
			}
			addDirectory(out, dir)
		}
	}
	if !statusSeen {
		return nil, fmt.Errorf("missing HTTP grpc status")
	}
	return out, nil
}
func converge(t *testing.T, m *model, f *harness.Fixture, cs []*harness.Clients) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	started := time.Now()
	var last error
	for {
		err := m.check(ctx, f, cs)
		if ctx.Err() == nil {
			last = err
		}
		if err == nil && ctx.Err() == nil {
			t.Logf("converged %d blobs / %d roots in %s", len(m.blobs), len(m.roots), time.Since(started))
			for i := 0; i < 2; i++ {
				time.Sleep(100 * time.Millisecond)
				require.NoError(t, m.check(ctx, f, cs), "converged state changed")
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("convergence: %v", last)
		case <-tick.C:
		}
	}
}
func TestStructureConvergence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--cache.avg_chunk_size_bytes=262144", "--cache.min_chunked_read_fallback_size_bytes=1048576", "--cache.directory_sizes_enabled=true"}, harness.CacheProxy: {"--cache.avg_chunk_size_bytes=262144", "--cache.min_chunked_read_fallback_size_bytes=1048576", "--cache_proxy.disable_cdc=false"}}})
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "structure-second", []string{"CACHE_WRITE"})
	cs := []*harness.Clients{f.Clients(harness.App), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, secondKey)}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	m := &model{blobs: map[string][]byte{}, dirs: map[string]*repb.Directory{}, mappings: map[string][]*repb.Digest{}}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for _, name := range []string{"first.txt", "nested/second.txt"} {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte("zip-" + name))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	m.put(t, ctx, cs[0], archive.Bytes(), false)
	converge(t, m, f, cs)
	for side, c := range cs {
		t.Run(fmt.Sprintf("publish-side-%d", side), func(t *testing.T) {
			// Supported SpliceBlob creates a manifest-only full blob, never uploaded flat.
			var full []byte
			var chunks []*repb.Digest
			for i := 0; i < 3; i++ {
				b := bytes.Repeat([]byte{byte(20 + side*3 + i)}, 1<<20)
				chunks = append(chunks, m.put(t, ctx, c, b, i%2 == 1))
				full = append(full, b...)
				converge(t, m, f, cs)
			}
			d := harness.Digest(full)
			for repeat := 0; repeat < 2; repeat++ {
				r, err := c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{InstanceName: instance, BlobDigest: d, ChunkDigests: chunks, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				require.True(t, proto.Equal(d, r.BlobDigest))
				m.blobs[key(d)] = full
				m.mappings[key(d)] = chunks
				converge(t, m, f, cs)
			}
			// These v2.13 RPCs are explicitly unimplemented in v2.314.0.
			// Use complete valid requests and pin that supported-surface boundary.
			for _, reader := range cs {
				mapping, err := reader.CAS.GetChunkMapping(ctx, &repb.GetChunkMappingRequest{InstanceName: instance, BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				_, err = mapping.Recv()
				require.Equal(t, codes.Unimplemented, status.Code(err))
				registration, err := reader.CAS.RegisterChunkMapping(ctx)
				require.NoError(t, err)
				err = registration.Send(&repb.RegisterChunkMappingRequest{InstanceName: instance, BlobDigest: d, ChunkDigests: chunks, DigestFunction: repb.DigestFunction_SHA256})
				require.True(t, err == nil || err == io.EOF)
				_, err = registration.CloseAndRecv()
				require.Equal(t, codes.Unimplemented, status.Code(err))
				converge(t, m, f, cs)
			}
			converge(t, m, f, cs)
			leaf := m.directory(t, ctx, c, &repb.Directory{Files: []*repb.FileNode{{Name: "payload", Digest: d, IsExecutable: true}}}, false)
			converge(t, m, f, cs)
			for revision := 0; revision < 5; revision++ {
				b := []byte(fmt.Sprintf("revision-%d-side-%d", revision, side))
				file := m.put(t, ctx, c, b, true)
				converge(t, m, f, cs)
				dir := &repb.Directory{Files: []*repb.FileNode{{Name: "revision", Digest: file}}, Directories: []*repb.DirectoryNode{{Name: "left", Digest: leaf}, {Name: "right", Digest: leaf}}}
				if revision == 3 {
					dir.Directories = dir.Directories[1:]
				}
				root := m.directory(t, ctx, c, dir, revision == 1)
				// directory() already publishes every Directory for all surfaces.
				converge(t, m, f, cs)
				require.True(t, proto.Equal(root, m.directory(t, ctx, c, dir, revision != 1)))
				converge(t, m, f, cs)

			}
		})
	}

}

// GetTree enumerates paths; a DAG node reachable through two edges can legally
// occur twice. Preserve and compare multiplicity instead of erasing duplicates.
func addDirectory(out map[string]*repb.Directory, dir *repb.Directory) {
	_, d, _ := harness.MarshalDigest(dir)
	base := key(d)
	k := base
	for n := 2; out[k] != nil; n++ {
		k = fmt.Sprintf("%s#%d", base, n)
	}
	out[k] = dir
}
func (m *model) expectedTree(d *repb.Digest, out map[string]*repb.Directory) {
	dir := m.dirs[key(d)]
	addDirectory(out, dir)
	for _, sub := range dir.Directories {
		m.expectedTree(sub.Digest, out)
	}
}
