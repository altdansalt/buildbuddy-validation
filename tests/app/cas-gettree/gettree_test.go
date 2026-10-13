package gettree

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var seed = flag.Int64("gettree.seed", 314159, "Replay seed")
var iterations = flag.Int("gettree.iterations", 24, "Bounded randomized cases per digest function")

type tree struct {
	root  *repb.Digest
	dirs  map[string]*repb.Directory
	blobs map[string][]byte
	fn    repb.DigestFunction_Value
}

func digest(b []byte, fn repb.DigestFunction_Value) *repb.Digest {
	var h []byte
	switch fn {
	case repb.DigestFunction_SHA384:
		s := sha512.Sum384(b)
		h = s[:]
	case repb.DigestFunction_SHA512:
		s := sha512.Sum512(b)
		h = s[:]
	case repb.DigestFunction_SHA1:
		s := sha1.Sum(b)
		h = s[:]
	case repb.DigestFunction_SHA256:
		s := sha256.Sum256(b)
		h = s[:]
	default:
		panic(fmt.Sprintf("unsupported digest function %s", fn))
	}
	return &repb.Digest{Hash: hex.EncodeToString(h), SizeBytes: int64(len(b))}
}
func key(d *repb.Digest) string { return fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes) }
func (tr *tree) add(t *testing.T, d *repb.Directory) *repb.Digest {
	t.Helper()
	b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	dg := digest(b, tr.fn)
	tr.dirs[key(dg)] = d
	tr.blobs[key(dg)] = b
	return dg
}
func newTree(fn repb.DigestFunction_Value) *tree {
	return &tree{fn: fn, dirs: map[string]*repb.Directory{}, blobs: map[string][]byte{}}
}
func generate(t *testing.T, r *rand.Rand, n int, fn repb.DigestFunction_Value) *tree {
	tr := newTree(fn)
	var nodes []*repb.Digest
	var shareable []*repb.Digest
	var expanded []int
	for i := 0; i < n; i++ {
		payload := []byte(fmt.Sprintf("payload-%d", i))
		tr.blobs[key(digest(payload, fn))] = payload
		d := &repb.Directory{Files: []*repb.FileNode{{Name: fmt.Sprintf("file-%04d", i), Digest: digest(payload, fn)}}}
		count := 0
		if i > 0 {
			count = 1 + r.Intn(6)
			if i == n-1 && n >= 100 {
				count = 128
			}
		}
		cost := 1
		for j := 0; j < count; j++ {
			// Bound expanded paths as well as unique nodes: GetTree may emit
			// duplicate directories, so unconstrained DAG edges grow exponentially.
			child := shareable[r.Intn(len(shareable))]
			childCost := 8
			if j == 0 {
				child = nodes[len(nodes)-1]
				childCost = expanded[len(expanded)-1]
			}
			cost += childCost
			d.Directories = append(d.Directories, &repb.DirectoryNode{Name: fmt.Sprintf("child-%04d", j), Digest: child})
		}
		dg := tr.add(t, d)
		nodes = append(nodes, dg)
		expanded = append(expanded, cost)
		if cost <= 8 {
			shareable = append(shareable, dg)
		}
	}
	tr.root = nodes[len(nodes)-1]
	// Reference model follows only available directory edges from the root.
	reachable := map[string]*repb.Directory{}
	var visit func(*repb.Digest)
	visit = func(d *repb.Digest) {
		k := key(d)
		if _, seen := reachable[k]; seen {
			return
		}
		if _, present := tr.blobs[k]; !present {
			return
		}
		dir := tr.dirs[k]
		reachable[k] = dir
		for _, child := range dir.Directories {
			visit(child.Digest)
		}
	}
	visit(tr.root)
	tr.dirs = reachable
	return tr
}
func upload(t *testing.T, ctx context.Context, c *harness.Clients, tr *tree, instance string) {
	t.Helper()
	if err := uploadBlobs(ctx, c, tr, instance); err != nil {
		t.Fatal(err)
	}
}
func uploadBlobs(ctx context.Context, c *harness.Clients, tr *tree, instance string) error {
	req := &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: tr.fn}
	for _, b := range tr.blobs {
		req.Requests = append(req.Requests, &repb.BatchUpdateBlobsRequest_Request{Digest: digest(b, tr.fn), Data: b})
	}
	res, err := c.CAS.BatchUpdateBlobs(ctx, req)
	if err != nil {
		return err
	}
	if len(res.Responses) != len(req.Requests) {
		return fmt.Errorf("upload response count: got %d want %d", len(res.Responses), len(req.Requests))
	}
	for _, r := range res.Responses {
		if r.GetStatus().GetCode() != 0 {
			return fmt.Errorf("upload: %v", r)
		}
	}
	return nil
}
func read(t *testing.T, ctx context.Context, c *harness.Clients, req *repb.GetTreeRequest) ([]*repb.GetTreeResponse, error) {
	t.Helper()
	s, err := c.CAS.GetTree(ctx, req)
	if err != nil {
		return nil, err
	}
	var out []*repb.GetTreeResponse
	for {
		p, err := s.Recv()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
}
func check(t *testing.T, pages []*repb.GetTreeResponse, tr *tree) {
	t.Helper()
	got := map[string]bool{}
	for _, p := range pages {
		for _, d := range p.Directories {
			b, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(d)
			k := key(digest(b, tr.fn))
			want, ok := tr.dirs[k]
			if !ok || !proto.Equal(want, d) {
				t.Fatalf("unexpected Directory %s", k)
			}
			got[k] = true
		}
	}
	// Compare against the independent availability-aware graph walk.
	if len(got) != len(tr.dirs) {
		t.Fatalf("reachable directory set: got %d want %d", len(got), len(tr.dirs))
	}
}
func TestSeededTrees(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	for _, fn := range []repb.DigestFunction_Value{repb.DigestFunction_SHA256, repb.DigestFunction_SHA1, repb.DigestFunction_SHA384, repb.DigestFunction_SHA512} {
		r := rand.New(rand.NewSource(*seed))
		for i := 0; i < *iterations; i++ {
			t.Run(fmt.Sprintf("seed=%d/%s/%d", *seed, fn, i), func(t *testing.T) {
				n := 1 + r.Intn(40)
				if i == 0 {
					n = 100
				}
				tr := generate(t, r, n, fn)
				instance := fmt.Sprintf("gettree/%s/%d", fn, i)
				upload(t, ctx, app, tr, instance)
				for _, c := range []*harness.Clients{app, proxy} {
					for repeat := 0; repeat < 2; repeat++ {
						req := &repb.GetTreeRequest{InstanceName: instance, RootDigest: tr.root, DigestFunction: fn}
						if repeat == 1 {
							req.PageSize = 10000
							if fn == repb.DigestFunction_SHA256 || fn == repb.DigestFunction_SHA1 {
								req.DigestFunction = repb.DigestFunction_UNKNOWN
							}
						}
						p, err := read(t, ctx, c, req)
						if err != nil {
							t.Fatal(err)
						}
						check(t, p, tr)
					}
				}
			})
		}
	}
	for _, c := range []*harness.Clients{app, proxy} {

		_, err := read(t, ctx, c, &repb.GetTreeRequest{})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("nil root: %v", err)
		}
	}
}
