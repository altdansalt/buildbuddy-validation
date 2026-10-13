package structures_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/protobuf/proto"
)

// Digest-function and instance-name variants of the same mutations. Every
// (function, instance) namespace is written through alternating endpoints and
// RPCs, and every namespace is compared with the model on both endpoints after
// every single step.

type variant struct {
	fn   repb.DigestFunction_Value
	name string
	inst string
}

func (v variant) digest(b []byte) *repb.Digest {
	var h []byte
	switch v.fn {
	case repb.DigestFunction_SHA1:
		s := sha1.Sum(b)
		h = s[:]
	case repb.DigestFunction_SHA384:
		s := sha512.Sum384(b)
		h = s[:]
	case repb.DigestFunction_SHA512:
		s := sha512.Sum512(b)
		h = s[:]
	default:
		s := sha256.Sum256(b)
		h = s[:]
	}
	return &repb.Digest{Hash: hex.EncodeToString(h), SizeBytes: int64(len(b))}
}

// rn builds a ByteStream resource name; SHA256 keeps the legacy form too.
func (v variant) rn(d *repb.Digest, compressed bool, upload bool) string {
	s := ""
	if v.inst != "" {
		s = v.inst + "/"
	}
	if upload {
		s += "uploads/" + uuid.NewString() + "/"
	}
	if compressed {
		s += "compressed-blobs/zstd/"
	} else {
		s += "blobs/"
	}
	if v.fn != repb.DigestFunction_SHA256 {
		s += v.name + "/"
	}
	return fmt.Sprintf("%s%s/%d", s, d.Hash, d.SizeBytes)
}

type vmodel struct {
	blobs map[string]map[string][]byte // variant -> digest key -> bytes
	dirs  map[string]map[string]*repb.Directory
	roots map[string][]*repb.Digest
}

func vkey(v variant) string { return v.name + "|" + v.inst }

func (m *vmodel) write(ctx context.Context, c *harness.Clients, v variant, b []byte, how string) (*repb.Digest, error) {
	d := v.digest(b)
	switch how {
	case "batch":
		r, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: v.inst, DigestFunction: v.fn, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: b}}})
		if err != nil {
			return nil, err
		}
		if len(r.Responses) != 1 || r.Responses[0].GetStatus().GetCode() != 0 {
			return nil, fmt.Errorf("batch update: %v", r)
		}
	case "stream", "zstd":
		data := b
		if how == "zstd" {
			var err error
			if data, err = harness.Compress(b); err != nil {
				return nil, err
			}
		}
		s, err := c.ByteStream.Write(ctx)
		if err != nil {
			return nil, err
		}
		if err := s.Send(&bspb.WriteRequest{ResourceName: v.rn(d, how == "zstd", true), Data: data, FinishWrite: true}); err != nil && err != io.EOF {
			return nil, err
		}
		if _, err := s.CloseAndRecv(); err != nil {
			return nil, err
		}
	}
	k := vkey(v)
	if m.blobs[k] == nil {
		m.blobs[k] = map[string][]byte{}
	}
	m.blobs[k][key(d)] = b
	return d, nil
}

func (m *vmodel) check(ctx context.Context, variants []variant, cs []*harness.Clients) error {
	for _, v := range variants {
		blobs := m.blobs[vkey(v)]
		keys := make([]string, 0, len(blobs))
		for k := range blobs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var all []*repb.Digest
		for _, k := range keys {
			all = append(all, v.digest(blobs[k]))
		}
		for ci, c := range cs {
			where := fmt.Sprintf("%s endpoint=%d", vkey(v), ci)
			if len(all) > 0 {
				miss, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: v.inst, BlobDigests: all, DigestFunction: v.fn})
				if err != nil {
					return fmt.Errorf("%s FindMissing: %w", where, err)
				}
				if len(miss.MissingBlobDigests) != 0 {
					return fmt.Errorf("%s FindMissing reports %d of %d modeled blobs missing", where, len(miss.MissingBlobDigests), len(all))
				}
				batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: v.inst, Digests: all, DigestFunction: v.fn})
				if err != nil {
					return fmt.Errorf("%s BatchRead: %w", where, err)
				}
				if len(batch.Responses) != len(all) {
					return fmt.Errorf("%s BatchRead returned %d responses for %d digests", where, len(batch.Responses), len(all))
				}
				got := map[string][]byte{}
				for _, r := range batch.Responses {
					if r.GetStatus().GetCode() != 0 {
						return fmt.Errorf("%s BatchRead %s status %v", where, key(r.Digest), r.Status)
					}
					got[key(r.Digest)] = r.Data
				}
				for _, k := range keys {
					if !bytes.Equal(got[k], blobs[k]) {
						return fmt.Errorf("%s BatchRead bytes differ for %s", where, k)
					}
				}
			}
			for _, k := range keys {
				want := blobs[k]
				d := v.digest(want)
				for _, compressed := range []bool{false, true} {
					s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: v.rn(d, compressed, false)})
					var got []byte
					for err == nil {
						var r *bspb.ReadResponse
						if r, err = s.Recv(); err == nil {
							got = append(got, r.Data...)
						}
					}
					if err != io.EOF {
						return fmt.Errorf("%s Read compressed=%t %s: %w", where, compressed, k, err)
					}
					if compressed {
						if got, err = harness.Decompress(got); err != nil {
							return err
						}
					}
					if !bytes.Equal(got, want) {
						return fmt.Errorf("%s Read compressed=%t bytes differ for %s", where, compressed, k)
					}
				}
				if len(want) > 2 {
					s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: v.rn(d, false, false), ReadOffset: 1, ReadLimit: int64(len(want) - 2)})
					var got []byte
					for err == nil {
						var r *bspb.ReadResponse
						if r, err = s.Recv(); err == nil {
							got = append(got, r.Data...)
						}
					}
					if err != io.EOF || !bytes.Equal(got, want[1:len(want)-1]) {
						return fmt.Errorf("%s ranged Read %s err=%v len=%d", where, k, err, len(got))
					}
				}
			}
			// Directory DAG for this namespace.
			for _, root := range m.roots[vkey(v)] {
				want := map[string]*repb.Directory{}
				m.expected(v, root, want)
				got, err := vtree(ctx, c, v, root)
				if err != nil {
					return fmt.Errorf("%s GetTree %s: %w", where, key(root), err)
				}
				if len(got) != len(want) {
					return fmt.Errorf("%s GetTree %s returned %d directories, want %d", where, key(root), len(got), len(want))
				}
				for k, dir := range want {
					if g := got[k]; g == nil || !proto.Equal(g, dir) {
						return fmt.Errorf("%s GetTree %s missing/different directory %s", where, key(root), k)
					}
				}
			}
		}
	}
	return nil
}

func vdirDigest(v variant, dir *repb.Directory) *repb.Digest {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(dir)
	return v.digest(b)
}

func (m *vmodel) expected(v variant, d *repb.Digest, out map[string]*repb.Directory) {
	dir := m.dirs[vkey(v)][key(d)]
	addDirectory(out, dir)
	for _, sub := range dir.Directories {
		m.expected(v, sub.Digest, out)
	}
}

func vtree(ctx context.Context, c *harness.Clients, v variant, root *repb.Digest) (map[string]*repb.Directory, error) {
	out := map[string]*repb.Directory{}
	token := ""
	for pages := 0; pages < 100; pages++ {
		s, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: v.inst, RootDigest: root, PageSize: 2, PageToken: token, DigestFunction: v.fn})
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
				addDirectory(out, dir)
			}
			token = r.NextPageToken
		}
		if token == "" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("too many pages")
}

func (m *vmodel) converge(t *testing.T, step string, variants []variant, cs []*harness.Clients) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	var last error
	for {
		err := m.check(ctx, variants, cs)
		if ctx.Err() == nil {
			last = err
		}
		if err == nil && ctx.Err() == nil {
			t.Logf("step %q converged in %s", step, time.Since(started))
			for i := 0; i < 2; i++ {
				time.Sleep(100 * time.Millisecond)
				require.NoError(t, m.check(ctx, variants, cs), "step %q: converged state changed", step)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("step %q: no convergence: %v", step, last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestDigestFunctionInstanceConvergence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "variants-second", []string{"CACHE_WRITE"})
	app, proxy := f.Clients(harness.App), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, secondKey)
	cs := []*harness.Clients{app, proxy}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var variants []variant
	for _, fn := range []struct {
		fn   repb.DigestFunction_Value
		name string
	}{{repb.DigestFunction_SHA256, "sha256"}, {repb.DigestFunction_SHA1, "sha1"}, {repb.DigestFunction_SHA384, "sha384"}, {repb.DigestFunction_SHA512, "sha512"}} {
		for _, inst := range []string{"", "team/a"} {
			variants = append(variants, variant{fn.fn, fn.name, inst})
		}
	}
	m := &vmodel{blobs: map[string]map[string][]byte{}, dirs: map[string]map[string]*repb.Directory{}, roots: map[string][]*repb.Digest{}}
	hows := []string{"batch", "stream", "zstd"}
	step := 0
	for vi, v := range variants {
		// Same payload bytes in every namespace: a namespace must not alias
		// another one's presence, and both endpoints must agree on each.
		payload := []byte(fmt.Sprintf("variant-payload-%s-%s", v.name, v.inst))
		for round, how := range hows {
			w := cs[(vi+round)%2]
			content := append(append([]byte{}, payload...), byte('0'+round))
			_, err := m.write(ctx, w, v, content, how)
			require.NoError(t, err)
			step++
			m.converge(t, fmt.Sprintf("%s write %s via endpoint %d", vkey(v), how, (vi+round)%2), variants, cs)
			// Repeat identical write through the other endpoint and RPC.
			_, err = m.write(ctx, cs[(vi+round+1)%2], v, content, hows[(round+1)%3])
			require.NoError(t, err)
			m.converge(t, fmt.Sprintf("%s repeat write", vkey(v)), variants, cs)
		}
		// Directory DAG in this namespace: leaf -> root revisions, alternating
		// the publishing endpoint; later revisions keep the leaf referenced twice.
		var leafFiles []*repb.FileNode
		for _, k := range sortedKeys(m.blobs[vkey(v)]) {
			leafFiles = append(leafFiles, &repb.FileNode{Name: "f" + hex.EncodeToString([]byte(k))[:6], Digest: v.digest(m.blobs[vkey(v)][k])})
			break
		}
		leaf := &repb.Directory{Files: leafFiles}
		if m.dirs[vkey(v)] == nil {
			m.dirs[vkey(v)] = map[string]*repb.Directory{}
		}
		for rev := 0; rev < 3; rev++ {
			ld := vdirDigest(v, leaf)
			lb, _ := proto.MarshalOptions{Deterministic: true}.Marshal(leaf)
			_, err := m.write(ctx, cs[(vi+rev)%2], v, lb, hows[rev%3])
			require.NoError(t, err)
			m.dirs[vkey(v)][key(ld)] = leaf
			m.converge(t, fmt.Sprintf("%s leaf rev %d", vkey(v), rev), variants, cs)
			root := &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "a", Digest: ld}, {Name: "b", Digest: ld}}, Files: []*repb.FileNode{{Name: fmt.Sprintf("rev%d", rev), Digest: v.digest([]byte("x"))}}}
			if rev == 2 {
				root.Directories = root.Directories[:1]
			}
			_, err = m.write(ctx, cs[(vi+rev+1)%2], v, []byte("x"), "batch")
			require.NoError(t, err)
			rb, _ := proto.MarshalOptions{Deterministic: true}.Marshal(root)
			rd, err := m.write(ctx, cs[(vi+rev+1)%2], v, rb, hows[(rev+1)%3])
			require.NoError(t, err)
			m.dirs[vkey(v)][key(rd)] = root
			m.roots[vkey(v)] = append(m.roots[vkey(v)], rd)
			m.converge(t, fmt.Sprintf("%s root rev %d", vkey(v), rev), variants, cs)
			// Next revision changes the leaf content set (update) via a new file.
			leaf = &repb.Directory{Files: append(append([]*repb.FileNode{}, leaf.Files...), &repb.FileNode{Name: fmt.Sprintf("extra%d", rev), Digest: v.digest([]byte("x")), IsExecutable: rev%2 == 0})}
			sort.Slice(leaf.Files, func(i, j int) bool { return leaf.Files[i].Name < leaf.Files[j].Name })
		}
	}
	t.Logf("%d steps, %d namespaces", step, len(variants))
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
