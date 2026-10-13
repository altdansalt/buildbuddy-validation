package gettree

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestEmptyDirectories(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			c := f.Clients(component)
			for _, rootEmpty := range []bool{true, false} {
				t.Run(map[bool]string{true: "empty-root", false: "empty-child"}[rootEmpty], func(t *testing.T) {
					tr := newTree(repb.DigestFunction_SHA256)
					empty := tr.add(t, &repb.Directory{})
					tr.root = empty
					if !rootEmpty {
						tr.root = tr.add(t, &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "empty", Digest: empty}}})
					}
					upload(t, ctx, c, tr, "empty")
					pages, err := read(t, ctx, c, &repb.GetTreeRequest{InstanceName: "empty", RootDigest: tr.root})
					if err != nil {
						t.Fatal(err)
					}
					check(t, pages, tr)
				})
			}
		})
	}
}

// GetTree may end a stream with a token and require another RPC. Assemble
// those streams before comparing token resumes against the complete suffix.
func readAllPages(t *testing.T, ctx context.Context, c *harness.Clients, req *repb.GetTreeRequest) ([]*repb.GetTreeResponse, error) {
	t.Helper()
	req = proto.Clone(req).(*repb.GetTreeRequest)
	seen := map[string]bool{}
	var all []*repb.GetTreeResponse
	for round := 0; round < 1024; round++ {
		pages, err := read(t, ctx, c, req)
		if err != nil {
			return all, err
		}
		all = append(all, pages...)
		if len(pages) == 0 || pages[len(pages)-1].NextPageToken == "" {
			return all, nil
		}
		token := pages[len(pages)-1].NextPageToken
		if seen[token] {
			return all, fmt.Errorf("pagination failed to advance: repeated token %q", token)
		}
		seen[token] = true
		req.PageToken = token
	}
	return all, fmt.Errorf("pagination exceeded bounded request count")
}

func TestPageSize(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			c := f.Clients(component)
			tr := newTree(repb.DigestFunction_SHA256)
			leaf := tr.add(t, &repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "target"}}})
			tr.root = tr.add(t, &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "child", Digest: leaf}}})
			upload(t, ctx, c, tr, "pages")
			req := &repb.GetTreeRequest{InstanceName: "pages", RootDigest: tr.root, PageSize: 1}
			pages, err := readAllPages(t, ctx, c, req)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range pages {
				if len(p.Directories) > 1 {
					t.Errorf("page_size=1: response contains %d directories; token=%q", len(p.Directories), p.NextPageToken)
				}
			}
			check(t, pages, tr)
			// A token must resume the same suffix even if traversal order varies between fresh calls.
			for i, p := range pages {
				if p.NextPageToken == "" {
					continue
				}
				req.PageToken = p.NextPageToken
				suffix, err := readAllPages(t, ctx, c, req)
				if err != nil {
					t.Fatal(err)
				}
				var expected, actual []*repb.Directory
				for _, later := range pages[i+1:] {
					expected = append(expected, later.Directories...)
				}
				for _, resumed := range suffix {
					actual = append(actual, resumed.Directories...)
				}
				if len(actual) != len(expected) {
					t.Fatalf("token %q: suffix length got %d want %d", req.PageToken, len(actual), len(expected))
				}
				for j := range expected {
					if !proto.Equal(actual[j], expected[j]) {
						t.Fatalf("token %q: suffix order/content differs at %d", req.PageToken, j)
					}
				}
			}
		})
	}
}

func TestMissingDirectories(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			c := f.Clients(component)
			t.Run("missing-root", func(t *testing.T) {
				_, err := read(t, ctx, c, &repb.GetTreeRequest{RootDigest: digest([]byte("missing-root"), repb.DigestFunction_SHA256)})
				if status.Code(err) != codes.NotFound {
					t.Fatalf("missing root: got %v want NotFound", err)
				}
			})
			t.Run("missing-child", func(t *testing.T) {
				tr := newTree(repb.DigestFunction_SHA256)
				tr.root = tr.add(t, &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "absent", Digest: digest([]byte("absent-directory"), tr.fn)}}})
				upload(t, ctx, c, tr, "missing")
				pages, err := read(t, ctx, c, &repb.GetTreeRequest{InstanceName: "missing", RootDigest: tr.root})
				if err != nil {
					t.Fatalf("missing child must be omitted, retaining root: %v", err)
				}
				check(t, pages, tr)
			})
		})
	}
}
