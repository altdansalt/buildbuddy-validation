package outputs_test

import (
	"flag"
	"fmt"
	"math/rand"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

var outputSeed = flag.Int64("outputs.seed", 20261013, "replay output declaration generator")
var outputCases = flag.Int("outputs.cases", 48, "number of generated actions")

type outputNode struct {
	kind, value string
	executable  bool
}

// The model is independent of the executor: it records the filesystem built by
// the generated shell command, then applies REAPI selection to that filesystem.
func TestSeededOutputDeclarations(t *testing.T) {
	r := start(t)
	rng := rand.New(rand.NewSource(*outputSeed))
	for i := 0; i < *outputCases; i++ {
		t.Run(fmt.Sprintf("seed-%d-case-%03d", *outputSeed, i), func(t *testing.T) {
			fs := map[string]outputNode{}
			script := []string{"rm -rf a b targets", "mkdir -p targets/dir", "printf target > targets/file"}
			addDir := func(p string) {
				for p != "." {
					fs[p] = outputNode{kind: "dir"}
					p = path.Dir(p)
				}
			}
			candidates := []string{"a", "a/empty", "a/nested", "a/nested/file", "a/link", "a/missing", "b", "b/empty", "b/nested", "b/nested/file", "b/link", "b/missing"}
			cmd := shell("")
			mode := i % 5 // modern, legacy, mixed files, mixed directories, all three
			for _, p := range candidates {
				if rng.Intn(2) == 0 {
					continue
				}
				if mode == 1 {
					if strings.HasSuffix(p, "file") || strings.HasSuffix(p, "link") || strings.HasSuffix(p, "missing") {
						cmd.OutputFiles = append(cmd.OutputFiles, p)
					} else {
						cmd.OutputDirectories = append(cmd.OutputDirectories, p)
					}
				} else {
					cmd.OutputPaths = append(cmd.OutputPaths, p)
				}
			}
			if mode != 1 && len(cmd.OutputPaths) == 0 {
				cmd.OutputPaths = []string{"a/missing"}
			}
			if mode == 2 {
				cmd.OutputFiles = []string{"b/link"}
			}
			if mode == 3 {
				cmd.OutputDirectories = []string{"b/empty"}
			}
			if mode == 4 {
				cmd.OutputFiles = []string{"b/link"}
				cmd.OutputDirectories = []string{"b/empty"}
				if rng.Intn(2) == 0 {
					cmd.OutputFiles = append(cmd.OutputFiles, "a/nested/file")
				}
				if rng.Intn(2) == 0 {
					cmd.OutputDirectories = append(cmd.OutputDirectories, "a/nested")
				}
			}
			// Reset the documented legacy precreation exception, then recreate only
			// declaration parents explicitly in the command. Missing legacy directories
			// now really are absent; ignored fields cannot manufacture output dirs.
			for _, list := range [][]string{cmd.OutputPaths, cmd.OutputFiles, cmd.OutputDirectories} {
				for _, p := range list {
					if parent := path.Dir(p); parent != "." {
						addDir(parent)
						script = append(script, "mkdir -p "+parent)
					}
				}
			}
			for _, root := range []string{"a", "b"} {
				for _, suffix := range []string{"empty", "nested", "nested/file", "link"} {
					p := root + "/" + suffix
					if rng.Intn(3) == 0 {
						continue
					}
					addDir(path.Dir(p))
					script = append(script, "mkdir -p "+path.Dir(p))
					switch suffix {
					case "empty", "nested":
						addDir(p)
						script = append(script, "mkdir -p "+p)
					case "nested/file":
						value := fmt.Sprintf("s%d-c%d-%s", *outputSeed, i, root)
						exec := rng.Intn(2) == 0
						fs[p] = outputNode{kind: "file", value: value, executable: exec}
						script = append(script, "printf '%s' '"+value+"' > "+p)
						if exec {
							script = append(script, "chmod 755 "+p)
						}
					case "link":
						target := []string{"../targets/file", "../targets/dir", "absent"}[rng.Intn(3)]
						fs[p] = outputNode{kind: "link", value: target}
						script = append(script, "ln -s "+target+" "+p)
					}
				}
			}
			sort.Strings(cmd.OutputPaths)
			sort.Strings(cmd.OutputFiles)
			sort.Strings(cmd.OutputDirectories)
			cmd.Arguments = shell(strings.Join(script, "; ")).Arguments
			t.Logf("replay: --test_arg=-outputs.seed=%d --test_arg=-outputs.cases=%d; command=%v", *outputSeed, *outputCases, cmd)
			result := success(t, r.execute(t, cmd, nil))
			selected := cmd.OutputPaths
			modern := len(selected) > 0
			if !modern {
				selected = append(append([]string{}, cmd.OutputFiles...), cmd.OutputDirectories...)
			}
			wantFiles, wantDirs := map[string]outputNode{}, map[string]outputNode{}
			wantLinks, wantFileLinks, wantDirLinks := map[string]string{}, map[string]string{}, map[string]string{}
			for _, p := range selected {
				n, ok := fs[p]
				if !ok {
					continue
				}
				switch n.kind {
				case "file":
					overlapped := false
					for _, ancestor := range selected {
						if fs[ancestor].kind == "dir" && strings.HasPrefix(p, ancestor+"/") {
							overlapped = true
						}
					}
					if overlapped {
						t.Logf("known exception overlapping-file: %s", p)
					} else {
						wantFiles[p] = n
					}
				case "dir":
					// Known legacy-type-mismatch finding: classify by actual type.
					if !modern && containsOutput(cmd.OutputFiles, p) {
						t.Logf("known exception legacy-type-mismatch: %s", p)
					}
					wantDirs[p] = n
				case "link":
					if modern && len(cmd.OutputFiles) == 0 && len(cmd.OutputDirectories) == 0 {
						wantLinks[p] = n.value
					} else {
						// Known mixed-field symlink-loss finding: only legacy membership
						// survives, in the legacy result field. Do not waive other outputs.
						if modern {
							t.Logf("known exception output-paths-symlink-loss: %s", p)
						}
						if containsOutput(cmd.OutputFiles, p) {
							wantFileLinks[p] = n.value
						}
						if containsOutput(cmd.OutputDirectories, p) {
							wantDirLinks[p] = n.value
						}
					}
				}
			}
			if modern && len(cmd.OutputFiles) > 0 {
				// Round 4 ignored-legacy-symlink repro: an ignored file
				// declaration leaks a top-level link from a selected Tree.
				for _, p := range cmd.OutputFiles {
					n := fs[p]
					if n.kind != "link" || containsOutput(cmd.OutputPaths, p) {
						continue
					}
					for _, ancestor := range cmd.OutputPaths {
						if fs[ancestor].kind == "dir" && strings.HasPrefix(p, ancestor+"/") {
							t.Logf("known exception ignored-legacy-symlink: %s", p)
							wantFileLinks[p] = n.value
						}
					}
				}
			}
			gotFiles := map[string]outputNode{}
			for _, f := range result.OutputFiles {
				require.NotContains(t, gotFiles, f.Path)
				gotFiles[f.Path] = outputNode{kind: "file", value: string(r.read(t, f.Digest)), executable: f.IsExecutable}
			}
			require.Equal(t, wantFiles, gotFiles)
			trees := map[string]*repb.Tree{}
			for _, d := range result.OutputDirectories {
				tree := &repb.Tree{}
				require.NoError(t, proto.Unmarshal(r.read(t, d.TreeDigest), tree))
				trees[d.Path] = tree
			}
			gotDirs := map[string]outputNode{}
			for _, d := range result.OutputDirectories {
				require.NotContains(t, gotDirs, d.Path)
				gotDirs[d.Path] = outputNode{kind: "dir"}
				tree := trees[d.Path]
				want := map[string]outputNode{}
				for p, n := range fs {
					if strings.HasPrefix(p, d.Path+"/") {
						want[strings.TrimPrefix(p, d.Path+"/")] = n
					}
				}
				require.Equal(t, want, r.flattenOutputTree(t, tree, d.Path, trees), "tree %s", d.Path)
			}
			require.Equal(t, wantDirs, gotDirs)
			checkOutputLinks(t, wantLinks, result.OutputSymlinks)
			if modern && len(cmd.OutputFiles) == 0 && len(cmd.OutputDirectories) == 0 {
				// REAPI permits optional v2.0 compatibility fields for modern
				// explicitly selected symlinks, classified by their target.
				for p, target := range wantLinks {
					switch target {
					case "../targets/file":
						wantFileLinks[p] = target
					case "../targets/dir":
						wantDirLinks[p] = target
					}
				}
				checkOptionalOutputLinks(t, wantFileLinks, result.OutputFileSymlinks)
				checkOptionalOutputLinks(t, wantDirLinks, result.OutputDirectorySymlinks)
			} else {
				checkOutputLinks(t, wantFileLinks, result.OutputFileSymlinks)
				checkOutputLinks(t, wantDirLinks, result.OutputDirectorySymlinks)
			}
		})
	}
}
func containsOutput(list []string, p string) bool {
	i := sort.SearchStrings(list, p)
	return i < len(list) && list[i] == p
}
func checkOutputLinks(t *testing.T, want map[string]string, list []*repb.OutputSymlink) {
	t.Helper()
	got := map[string]string{}
	for _, s := range list {
		require.NotContains(t, got, s.Path)
		got[s.Path] = s.Target
	}
	require.Equal(t, want, got)
}
func (r runner) flattenOutputTree(t *testing.T, tree *repb.Tree, rootPath string, trees map[string]*repb.Tree) map[string]outputNode {
	t.Helper()
	require.NotNil(t, tree.Root)
	children := map[string]*repb.Directory{}
	for _, d := range tree.Children {
		_, digest, err := harness.MarshalDigest(d)
		require.NoError(t, err)
		children[digest.Hash] = d
	}
	got := map[string]outputNode{}
	var walk func(string, *repb.Directory, int)
	walk = func(prefix string, d *repb.Directory, depth int) {
		require.Less(t, depth, 20, "cyclic or unexpectedly deep output Tree")
		add := func(name string, n outputNode) {
			p := path.Join(prefix, name)
			require.NotContains(t, got, p)
			got[p] = n
		}
		for _, f := range d.Files {
			add(f.Name, outputNode{kind: "file", value: string(r.read(t, f.Digest)), executable: f.IsExecutable})
		}
		for _, s := range d.Symlinks {
			add(s.Name, outputNode{kind: "link", value: s.Target})
		}
		for _, child := range d.Directories {
			add(child.Name, outputNode{kind: "dir"})
			node := children[child.Digest.Hash]
			if node == nil {
				fullPath := path.Join(rootPath, prefix, child.Name)
				// Confirmed overlapping-directory-tree exception: repair only
				// from a separately declared descendant Tree, by exact digest.
				for outputPath, other := range trees {
					if !strings.HasPrefix(outputPath, rootPath+"/") || !(fullPath == outputPath || strings.HasPrefix(fullPath, outputPath+"/")) {
						continue
					}
					nodes := append([]*repb.Directory{other.Root}, other.Children...)
					for _, candidate := range nodes {
						_, digest, err := harness.MarshalDigest(candidate)
						require.NoError(t, err)
						if proto.Equal(digest, child.Digest) {
							node = candidate
							t.Logf("known exception overlapping-directory-tree: %s", fullPath)
							break
						}
					}
					if node != nil {
						break
					}
				}
			}
			require.NotNil(t, node, "missing Tree child %v", child.Digest)
			_, digest, err := harness.MarshalDigest(node)
			require.NoError(t, err)
			require.True(t, proto.Equal(digest, child.Digest))
			walk(path.Join(prefix, child.Name), node, depth+1)
		}
	}
	walk("", tree.Root, 0)
	return got
}

func checkOptionalOutputLinks(t *testing.T, allowed map[string]string, list []*repb.OutputSymlink) {
	t.Helper()
	seen := map[string]bool{}
	for _, s := range list {
		require.False(t, seen[s.Path], "duplicate compatibility symlink %s", s.Path)
		seen[s.Path] = true
		require.Contains(t, allowed, s.Path)
		require.Equal(t, allowed[s.Path], s.Target)
	}
}
