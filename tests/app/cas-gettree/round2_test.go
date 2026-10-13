package gettree

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"
)

func TestWideDirectory(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tr := newTree(repb.DigestFunction_SHA256)
	leaf := tr.add(t, &repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "target"}}})
	root := &repb.Directory{}
	payload := []byte("wide-file")
	tr.blobs[key(digest(payload, tr.fn))] = payload
	for i := 0; i < 30000; i++ {
		root.Directories = append(root.Directories, &repb.DirectoryNode{Name: fmt.Sprintf("dir-%05d", i), Digest: leaf})
		root.Files = append(root.Files, &repb.FileNode{Name: fmt.Sprintf("file-%05d", i), Digest: digest(payload, tr.fn)})
	}
	tr.root = tr.add(t, root)
	t.Logf("root bytes=%d directory entries=%d file entries=%d", tr.root.SizeBytes, len(root.Directories), len(root.Files))
	// The root exceeds the usual batch-upload limit; use streaming ByteStream.
	for _, b := range tr.blobs {
		got, err := f.Clients(harness.App).Write(ctx, "wide", b, false)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got, digest(b, tr.fn)) {
			t.Fatal("upload digest mismatch")
		}
	}
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			for i := 0; i < 2; i++ {
				pages, err := read(t, ctx, f.Clients(component), &repb.GetTreeRequest{InstanceName: "wide", RootDigest: tr.root})
				if err != nil {
					t.Fatal(err)
				}
				check(t, pages, tr)
			}
		})
	}
}

func TestConcurrentGetTreeDuringReupload(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tr := generate(t, rand.New(rand.NewSource(*seed)), 100, repb.DigestFunction_SHA256)
	t.Logf("seed=%d readers=8 uploads=24", *seed)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	upload(t, ctx, app, tr, "concurrent")
	// A shared barrier overlaps eight readers with writes of the same immutable
	// blobs, alternating direct and proxy writes. Every read must stay complete.
	start := make(chan struct{})
	type result struct {
		pages         []*repb.GetTreeResponse
		err           error
		worker, round int
	}
	results := make(chan result, 8*24+1)
	var wg sync.WaitGroup
	for worker := 0; worker < 9; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			if worker == 8 {
				for i := 0; i < 24; i++ {
					c := app
					if i%2 == 1 {
						c = proxy
					}
					if err := uploadBlobs(ctx, c, tr, "concurrent"); err != nil {
						results <- result{err: err, worker: worker, round: i}
						return
					}
				}
				return
			}
			c := app
			if worker%2 == 1 {
				c = proxy
			}
			for i := 0; i < 24; i++ {
				pages, err := read(t, ctx, c, &repb.GetTreeRequest{InstanceName: "concurrent", RootDigest: tr.root})
				if err != nil {
					results <- result{err: err, worker: worker, round: i}
					return
				}
				results <- result{pages: pages, worker: worker, round: i}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	close(results)
	for r := range results {
		if r.err != nil {
			t.Errorf("worker=%d round=%d: %v", r.worker, r.round, r.err)
			continue
		}
		check(t, r.pages, tr)
	}
}

// These serialized Directory vectors and BLAKE3 hashes are computed independently
// with Python blake3 1.0.11, so this test requires no shared dependency change.
func TestBLAKE3GetTree(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	vectors := []struct{ data, hash string }{
		{"1a0e0a046c696e6b1206746172676574", "8551133cf3c17a9891ca611f26a0f83583c8394b3cc2e73e037426e4bcdbf93f"},
		{"124d0a056368696c6412440a40383535313133336366336331376139383931636136313166323661306638333538336338333934623363633265373365303337343236653462636462663933661010", "fb7f07ce5dc9ce33445e24f7db08f710ae3839ef66069d46e83f454d6076303d"},
		{"12520a0a726f6f742d6368696c6412440a4066623766303763653564633963653333343435653234663764623038663731306165333833396566363630363964343665383366343534643630373633303364104f", "53ee00d3cf36eb9bd699a4068e923061978254d4d9dda996ba0cbaf67098e6be"},
	}
	req := &repb.BatchUpdateBlobsRequest{InstanceName: "blake3", DigestFunction: repb.DigestFunction_BLAKE3}
	expected := map[string]*repb.Directory{}
	var root *repb.Digest
	for _, v := range vectors {
		b, err := hex.DecodeString(v.data)
		if err != nil {
			t.Fatal(err)
		}
		d := &repb.Directory{}
		if err := proto.Unmarshal(b, d); err != nil {
			t.Fatal(err)
		}
		root = &repb.Digest{Hash: v.hash, SizeBytes: int64(len(b))}
		expected[v.hash] = d
		req.Requests = append(req.Requests, &repb.BatchUpdateBlobsRequest_Request{Digest: root, Data: b})
	}
	res, err := f.Clients(harness.App).CAS.BatchUpdateBlobs(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Responses) != len(vectors) {
		t.Fatal("upload response count")
	}
	for _, r := range res.Responses {
		if r.GetStatus().GetCode() != 0 {
			t.Fatalf("upload: %v", r)
		}
	}
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			for repeat := 0; repeat < 2; repeat++ {
				pages, err := read(t, ctx, f.Clients(component), &repb.GetTreeRequest{InstanceName: "blake3", RootDigest: root, DigestFunction: repb.DigestFunction_BLAKE3})
				if err != nil {
					t.Fatal(err)
				}
				seen := map[string]bool{}
				for _, p := range pages {
					for _, d := range p.Directories {
						found := false
						for h, want := range expected {
							if proto.Equal(d, want) {
								seen[h] = true
								found = true
								break
							}
						}
						if !found {
							t.Fatalf("unexpected directory: %v", d)
						}
					}
				}
				if len(seen) != len(expected) {
					t.Fatalf("directories got=%d want=%d", len(seen), len(expected))
				}
			}
		})
	}
}
