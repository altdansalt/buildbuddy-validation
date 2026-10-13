package randtrees_test

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

var seed = flag.Int64("trees.seed", 314159, "Replay seed for randomized trees")
var iterations = flag.Int("trees.iterations", 16, "Number of trees (each action bounded to 20s)")

type node struct {
	name, target string
	data         []byte
	executable   bool
	children     []*node
	directory    bool
}

func generate(r *rand.Rand, depth int) *node {
	n := &node{directory: true}
	// Mandatory coverage also makes every random sample useful.
	n.children = append(n.children, &node{name: "empty dir", directory: true}, &node{name: "zero", data: []byte{}}, &node{name: "duplicate", data: []byte("shared contents\n"), executable: true}, &node{name: "duplicate nonexec", data: []byte("shared contents\n")}, &node{name: "link →", target: "duplicate"}, &node{name: "dangling", target: "missing target"}, &node{name: "directory link", target: "empty dir"})
	names := []string{"雪", "café", "e\u0301", "-dash", "quote'\"", "line\nbreak", "tab\tname", "back\\slash", "🙂", " space "}
	for i := 0; i < 1+r.Intn(7); i++ {
		child := &node{name: fmt.Sprintf("%02d-%s", i, names[r.Intn(len(names))])}
		if depth > 0 && r.Intn(3) == 0 {
			child = generate(r, depth-1)
			child.name = fmt.Sprintf("%02d-%s", i, names[r.Intn(len(names))])
		} else {
			child.data = make([]byte, r.Intn(257))
			_, _ = r.Read(child.data)
			child.executable = r.Intn(2) == 0
			if r.Intn(3) == 0 {
				child.data = []byte("shared contents\n")
			}
		}
		n.children = append(n.children, child)
	}
	sort.Slice(n.children, func(i, j int) bool { return n.children[i].name < n.children[j].name })
	return n
}
func upload(t *testing.T, ctx context.Context, c *harness.Clients, instance string, n *node) *repb.Digest {
	d := &repb.Directory{}
	for _, x := range n.children {
		if x.directory {
			d.Directories = append(d.Directories, &repb.DirectoryNode{Name: x.name, Digest: upload(t, ctx, c, instance, x)})
		} else if x.target != "" {
			d.Symlinks = append(d.Symlinks, &repb.SymlinkNode{Name: x.name, Target: x.target})
		} else {
			digest, err := c.Upload(ctx, instance, x.data)
			require.NoError(t, err)
			d.Files = append(d.Files, &repb.FileNode{Name: x.name, Digest: digest, IsExecutable: x.executable})
		}
	}
	digest, err := c.UploadProto(ctx, instance, d)
	require.NoError(t, err)
	return digest
}
func b64(s string) string                    { return base64.StdEncoding.EncodeToString([]byte(s)) }
func record(kind, path, value string) string { return kind + "\t" + b64(path) + "\t" + value }
func expected(n *node, path string, lines *[]string) {
	if n.directory {
		*lines = append(*lines, record("d", path, ""))
		for _, x := range n.children {
			expected(x, path+"/"+x.name, lines)
		}
	} else if n.target != "" {
		*lines = append(*lines, record("l", path, b64(n.target)))
	} else {
		*lines = append(*lines, record("f", path, fmt.Sprintf("%t:%s:%d", n.executable, harness.Digest(n.data).Hash, len(n.data))))
	}
}

const script = `set -eu
(cd input; find . -exec /bin/sh -c '
set -eu
for p do
 name=$(printf "%s" "$p" | base64 -w0)
 if [ -L "$p" ]; then printf "l\t%s\t%s\n" "$name" "$(readlink -n -- "$p" | base64 -w0)"
 elif [ -d "$p" ]; then printf "d\t%s\t\n" "$name"
 else
 executable=false; if [ -x "$p" ]; then executable=true; fi
 hash=$(sha256sum < "$p"); hash=${hash%% *}
 size=$(wc -c < "$p" | tr -d " ")
 printf "f\t%s\t%s:%s:%s\n" "$name" "$executable" "$hash" "$size"
 fi
done' sh {} +) > manifest
cp -a input output
`

func outputManifest(t *testing.T, ctx context.Context, c *harness.Clients, instance string, tree *repb.Tree) []string {
	dirs := map[string]*repb.Directory{}
	for _, d := range tree.Children {
		_, digest, err := harness.MarshalDigest(d)
		require.NoError(t, err)
		dirs[digest.Hash] = d
	}
	var lines []string
	var walk func(*repb.Directory, string, int)
	walk = func(d *repb.Directory, path string, depth int) {
		require.Less(t, depth, 20, "output cycle or excessive depth")
		lines = append(lines, record("d", path, ""))
		seen := map[string]bool{}
		check := func(name string) {
			require.NotEmpty(t, name)
			require.NotContains(t, name, "/")
			require.NotEqual(t, ".", name)
			require.NotEqual(t, "..", name)
			require.False(t, seen[name], "duplicate node %q", name)
			seen[name] = true
		}
		for _, f := range d.Files {
			check(f.Name)
			data, err := c.Read(ctx, instance, f.Digest)
			require.NoError(t, err)
			require.True(t, proto.Equal(harness.Digest(data), f.Digest))
			lines = append(lines, record("f", path+"/"+f.Name, fmt.Sprintf("%t:%s:%d", f.IsExecutable, f.Digest.Hash, f.Digest.SizeBytes)))
		}
		for _, s := range d.Symlinks {
			check(s.Name)
			lines = append(lines, record("l", path+"/"+s.Name, b64(s.Target)))
		}
		for _, child := range d.Directories {
			check(child.Name)
			next := dirs[child.Digest.Hash]
			require.NotNil(t, next, "missing output directory %s", child.Name)
			walk(next, path+"/"+child.Name, depth+1)
		}
	}
	require.NotNil(t, tree.Root)
	walk(tree.Root, ".", 0)
	sort.Strings(lines)
	return lines
}
func TestRandomTreeRoundTrip(t *testing.T) {
	t.Logf("replay: -trees.seed=%d -trees.iterations=%d", *seed, *iterations)
	require.Greater(t, *iterations, 0)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	r := rand.New(rand.NewSource(*seed))
	for i := 0; i < *iterations; i++ {
		t.Run(fmt.Sprintf("seed_%d_iteration_%d", *seed, i), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			instance := "rand-trees"
			n := generate(r, 1+r.Intn(3))
			var want []string
			expected(n, ".", &want)
			sort.Strings(want)
			input := upload(t, ctx, c, instance, n)
			root, err := c.UploadProto(ctx, instance, &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "input", Digest: input}}})
			require.NoError(t, err)
			cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, OutputPaths: []string{"manifest", "output"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
			require.NoError(t, err)
			action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(20 * time.Second), DoNotCache: true})
			require.NoError(t, err)
			stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			response := &repb.ExecuteResponse{}
			for {
				op, err := stream.Recv()
				require.NoError(t, err)
				if !op.Done {
					continue
				}
				require.Nil(t, op.GetError())
				require.NoError(t, op.GetResponse().UnmarshalTo(response))
				break
			}
			require.Zero(t, response.GetStatus().GetCode(), "%v", response.GetStatus())
			result := response.GetResult()
			require.NotNil(t, result)
			require.Zero(t, result.ExitCode, "%v", result)
			require.Len(t, result.OutputFiles, 1)
			require.Equal(t, "manifest", result.OutputFiles[0].Path)
			manifest, err := c.Read(ctx, instance, result.OutputFiles[0].Digest)
			require.NoError(t, err)
			got := strings.Split(strings.TrimSuffix(string(manifest), "\n"), "\n")
			sort.Strings(got)
			require.Equal(t, want, got, "input materialization; seed=%d iteration=%d", *seed, i)
			require.Len(t, result.OutputDirectories, 1)
			require.Equal(t, "output", result.OutputDirectories[0].Path)
			bytes, err := c.Read(ctx, instance, result.OutputDirectories[0].TreeDigest)
			require.NoError(t, err)
			tree := &repb.Tree{}
			require.NoError(t, proto.Unmarshal(bytes, tree))
			require.Equal(t, want, outputManifest(t, ctx, c, instance, tree), "output capture; seed=%d iteration=%d", *seed, i)
		})
	}
}
