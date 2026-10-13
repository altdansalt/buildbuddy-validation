package outputs_test

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

var outputSeed = flag.Int64("outputs.seed", 314159, "Replay seed for output declarations")
var outputIterations = flag.Int("outputs.iterations", 24, "Number of randomized actions per isolation mode")

// Model records all returned paths, bytes, executable bits and symlink targets.
// Directory traversal resolves every child digest inside its own Tree, detecting
// incomplete Trees even when the corresponding Directory happens to be in CAS.
func snapshot(t *testing.T, r runner, result *repb.ActionResult) map[string]string {
	t.Helper()
	got := map[string]string{}
	put := func(path, value string) { require.NotContains(t, got, path); got[path] = value }
	for _, file := range result.OutputFiles {
		put(file.Path, fmt.Sprintf("file:%t:%s", file.IsExecutable, r.read(t, file.Digest)))
	}
	for _, link := range result.OutputSymlinks {
		put(link.Path, "link:"+link.Target)
	}
	require.Empty(t, result.OutputFileSymlinks)
	require.Empty(t, result.OutputDirectorySymlinks)
	for _, out := range result.OutputDirectories {
		tree := &repb.Tree{}
		require.NoError(t, proto.Unmarshal(r.read(t, out.TreeDigest), tree))
		dirs := map[string]*repb.Directory{}
		for _, d := range tree.Children {
			_, digest, err := harness.MarshalDigest(d)
			require.NoError(t, err)
			dirs[digest.Hash] = d
		}
		var walk func(string, *repb.Directory)
		walk = func(path string, d *repb.Directory) {
			require.NotNil(t, d, "Tree missing child at %s", path)
			put(path, "dir")
			for _, file := range d.Files {
				put(path+"/"+file.Name, fmt.Sprintf("file:%t:%s", file.IsExecutable, r.read(t, file.Digest)))
			}
			for _, link := range d.Symlinks {
				put(path+"/"+link.Name, "link:"+link.Target)
			}
			for _, child := range d.Directories {
				walk(path+"/"+child.Name, dirs[child.Digest.Hash])
			}
		}
		walk(out.Path, tree.Root)
	}
	return got
}

func TestSeededOutputDeclarations(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *outputSeed, *outputIterations)
	require.Greater(t, *outputIterations, 0)
	rng := rand.New(rand.NewSource(*outputSeed))
	r := start(t)
	bare := r
	bare.isolation = "none"
	for iteration := 0; iteration < *outputIterations; iteration++ {
		t.Run(fmt.Sprintf("seed=%d/iteration=%d", *outputSeed, iteration), func(t *testing.T) {
			modern := rng.Intn(2) == 0
			cmd := shell("")
			scripts := []string{"umask 022", "printf ignored > undeclared"}
			expected := map[string]string{}
			paths := []string{}
			files, directories := []string{}, []string{}
			for i := 0; i < 1+rng.Intn(7); i++ {
				path := fmt.Sprintf("p%d", i)
				if rng.Intn(2) == 0 {
					path = "parent/" + path
				}
				scripts = append(scripts, "mkdir -p parent")
				content := strings.Repeat(fmt.Sprintf("s%d-i%d-n%d", *outputSeed, iteration, i), rng.Intn(8))
				switch rng.Intn(4) {
				case 0:
					executable := rng.Intn(2) == 0
					scripts = append(scripts, fmt.Sprintf("printf '%%s' '%s' > %s", content, path))
					if executable {
						scripts = append(scripts, "chmod 755 "+path)
					}
					expected[path] = fmt.Sprintf("file:%t:%s", executable, content)
					files = append(files, path)
				case 1:
					scripts = append(scripts, "mkdir -p "+path+"/nested/empty", fmt.Sprintf("printf '%%s' '%s' > %s/nested/file", content, path), "ln -s absent "+path+"/dangling")
					expected[path] = "dir"
					expected[path+"/nested"] = "dir"
					expected[path+"/nested/empty"] = "dir"
					expected[path+"/nested/file"] = "file:false:" + content
					expected[path+"/dangling"] = "link:absent"
					directories = append(directories, path)
				case 2:
					if modern {
						scripts = append(scripts, "ln -s absent "+path)
						expected[path] = "link:absent"
					} else {
						files = append(files, path)
					}
				case 3:
					files = append(files, path) // Intentionally absent; do not use legacy directories (precreated).
				}
				paths = append(paths, path)
			}
			sort.Strings(paths)
			sort.Strings(files)
			sort.Strings(directories)
			if modern {
				cmd.OutputPaths = paths
			} else {
				cmd.OutputFiles = files
				cmd.OutputDirectories = directories
			}
			cmd.Arguments = shell(strings.Join(scripts, "; ")).Arguments
			dockerResult := snapshot(t, r, success(t, r.execute(t, cmd, nil)))
			bareResult := snapshot(t, bare, success(t, bare.execute(t, proto.Clone(cmd).(*repb.Command), nil)))
			require.Equal(t, expected, dockerResult, "seed=%d iteration=%d script=%s", *outputSeed, iteration, cmd.Arguments)
			require.Equal(t, expected, bareResult, "seed=%d iteration=%d bare reference", *outputSeed, iteration)
			require.Equal(t, bareResult, dockerResult, "seed=%d iteration=%d isolation mismatch", *outputSeed, iteration)
		})
	}
}
