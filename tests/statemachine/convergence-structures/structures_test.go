package structures_test

import (
	"bytes"
	"context"
	"encoding/binary"
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
	add := func(dir *repb.Directory) { _, dg, _ := harness.MarshalDigest(dir); out[key(dg)] = dir }
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
			m.reachable(root, want)
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
		m.reachable(root, want)
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
		req := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "instanceName": instance, "rootDigest": map[string]any{"hash": root.Hash, "sizeBytes": strconv.FormatInt(root.SizeBytes, 10)}, "digestFunction": "SHA256"}
		res, err := f.Web.RPC("GetTreeDirectorySizes", req)
		if err != nil {
			return err
		}
		entries, _ := res["sizes"].([]any)
		sizes := map[string][2]int64{}
		for _, entry := range entries {
			v := entry.(map[string]any)
			parse := func(x any) int64 { n, _ := strconv.ParseInt(fmt.Sprint(x), 10, 64); return n }
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
			_, dg, _ := harness.MarshalDigest(dir)
			out[key(dg)] = dir
		}
	}
	if !statusSeen {
		return nil, fmt.Errorf("missing HTTP grpc status")
	}
	return out, nil
}
func converge(t *testing.T, m *model, f *harness.Fixture, cs []*harness.Clients) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var last error
	for {
		last = m.check(ctx, f, cs)
		if last == nil {
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
	cs := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m := &model{blobs: map[string][]byte{}, dirs: map[string]*repb.Directory{}, mappings: map[string][]*repb.Digest{}}
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
			}
			converge(t, m, f, cs)
			leaf := m.directory(t, ctx, c, &repb.Directory{Files: []*repb.FileNode{{Name: "payload", Digest: d, IsExecutable: true}}}, false)
			converge(t, m, f, cs)
			for revision := 0; revision < 2; revision++ {
				b := []byte(fmt.Sprintf("revision-%d-side-%d", revision, side))
				file := m.put(t, ctx, c, b, true)
				converge(t, m, f, cs)
				dir := &repb.Directory{Files: []*repb.FileNode{{Name: "revision", Digest: file}}, Directories: []*repb.DirectoryNode{{Name: "left", Digest: leaf}, {Name: "right", Digest: leaf}}}
				if revision == 1 {
					dir.Directories = dir.Directories[1:]
				}
				root := m.directory(t, ctx, c, dir, revision == 1)
				m.roots = append(m.roots, root)
				converge(t, m, f, cs)
				require.True(t, proto.Equal(root, m.directory(t, ctx, c, dir, revision != 1)))
				converge(t, m, f, cs)
				if bb, err := exec.LookPath("bb"); err == nil {
					for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
						out := t.TempDir()
						cmd := exec.CommandContext(ctx, bb, "download", key(root), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_directory="+out)
						cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/local/bin:/usr/bin:/bin"}
						log, err := cmd.CombinedOutput()
						require.NoError(t, err, string(log))
						expected := map[string][]byte{"revision": b, "right/payload": full}
						if revision == 0 {
							expected["left/payload"] = full
						} else {
							_, err := os.Stat(filepath.Join(out, "left"))
							require.True(t, os.IsNotExist(err))
						}
						for path, want := range expected {
							got, err := os.ReadFile(filepath.Join(out, path))
							require.NoError(t, err)
							require.Equal(t, want, got)
						}
					}
				} else {
					t.Log("bb CLI unavailable on runner; RPC and UI checks still required")
				}
			}
		})
	}

}
