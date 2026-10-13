package inputtree_test

import (
	"context"
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
)

var treeSeed = flag.Int64("inputs-docker.seed", 314159, "replayable input tree seed")
var treeIterations = flag.Int("inputs-docker.iterations", 6, "number of generated trees")

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// The model records every directory entry, content and executable bit before
// uploading. Per-entry assertions plus an exact entry count detect extra paths.
func TestRandomTree(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *treeSeed, *treeIterations)
	f := fixture(t)
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for iteration := 0; iteration < *treeIterations; iteration++ {
		seed := *treeSeed + int64(iteration)
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Logf("replay: -inputs-docker.seed=%d -inputs-docker.iterations=1", seed)
			rng := rand.New(rand.NewSource(seed))
			var blobs []*repb.Digest
			var contents []string
			for i := 0; i < 4; i++ {
				content := fmt.Sprintf("seed-%d-content-%d-%d", seed, i, rng.Int63())
				d, err := c.Upload(ctx, "input-tree", []byte(content))
				require.NoError(t, err)
				blobs = append(blobs, d)
				contents = append(contents, content)
			}
			entries := 0
			var checks strings.Builder
			var build func(string, int) *repb.Digest
			build = func(path string, depth int) *repb.Digest {
				dir := &repb.Directory{}
				// Include empty directories and mixed executable modes for repeated blobs.
				count := 0
				if depth < 3 {
					count = 8 + rng.Intn(24)
				}
				if depth == 0 {
					count = 256 + rng.Intn(256)
				}
				for i := 0; i < count; i++ {
					name := fmt.Sprintf("f%04d-%s", i, []string{"λ", "日本", "😀", "a b", "quote'", "line\nbreak"}[rng.Intn(6)])
					k := rng.Intn(len(blobs))
					executable := rng.Intn(2) == 0
					dir.Files = append(dir.Files, &repb.FileNode{Name: name, Digest: blobs[k], IsExecutable: executable})
					q := quote(path + name)
					entries++
					fmt.Fprintf(&checks, "test -f %s && test ! -L %s\ntest \"$(cat %s)\" = %s\n", q, q, q, quote(contents[k]))
					if executable {
						fmt.Fprintf(&checks, "test -x %s\n", q)
					} else {
						fmt.Fprintf(&checks, "test ! -x %s\n", q)
					}
				}
				if depth < 3 {
					for i := 0; i < 2+rng.Intn(3); i++ {
						name := fmt.Sprintf("d%d-é", i)
						entries++
						fmt.Fprintf(&checks, "test -d %s && test ! -L %s\n", quote(path+name), quote(path+name))
						d := build(path+name+"/", depth+1)
						dir.Directories = append(dir.Directories, &repb.DirectoryNode{Name: name, Digest: d})
					}
				}
				if count > 0 {
					for i, target := range []string{dir.Files[0].Name, "missing", "self"} {
						name := fmt.Sprintf("link%d", i)
						if i == 2 {
							target = name
						}
						entries++
						dir.Symlinks = append(dir.Symlinks, &repb.SymlinkNode{Name: name, Target: target})
						fmt.Fprintf(&checks, "test -L %s\ntest \"$(readlink %s)\" = %s\n", quote(path+name), quote(path+name), quote(target))
					}
				}
				sort.Slice(dir.Files, func(i, j int) bool { return dir.Files[i].Name < dir.Files[j].Name })
				d, err := c.UploadProto(ctx, "input-tree", dir)
				require.NoError(t, err)
				return d
			}
			rd := build("./", 0)
			raw, err := c.Read(ctx, "input-tree", rd)
			require.NoError(t, err)
			root := &repb.Directory{}
			require.NoError(t, proto.Unmarshal(raw, root))
			// Names may contain newlines: count NUL separators rather than text lines.
			script := checks.String() + fmt.Sprintf("test \"$(find . -mindepth 1 -print0 | tr -cd '\\000' | wc -c)\" -eq %d\n", entries)
			for pass := 0; pass < 2; pass++ {
				success(t, c, root, fmt.Sprintf("# seed=%d pass=%d\n", seed, pass)+script)
			}
		})
	}
}
