package randexec_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
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

var seed = flag.Int64("rand-exec.seed", 314159, "Deterministic generator seed")
var iterations = flag.Int("rand-exec.iterations", 16, "Number of cases (successful cases perform three executions)")

type file struct {
	Content    string
	Executable bool
}
type snapshot struct {
	Exit           int32
	Stdout, Stderr string
	Files          map[string]file
	Dirs           map[string]bool
	Links          map[string]string
}
type testCase struct {
	Command *repb.Command
	Input   string
}

func token(r *rand.Rand) string {
	alphabet := []rune("abcXYZ019 _-'%λ")
	b := make([]rune, r.Intn(40))
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

// Only fixed paths and quoted positional/environment expansions enter the script.
// No generated text is interpreted as shell syntax.
func generate(r *rand.Rand, index int) testCase {
	outputs := []string{"missing", "result.txt", "tree"}
	script := "set -eu\nmkdir -p tree/empty tree/nested\nprintf '%s\\n' \"$VALUE\" \"$1\" > result.txt\n"
	n := 1 + r.Intn(8)
	for i := 0; i < n; i++ {
		switch r.Intn(5) {
		case 0:
			script += "cat input.txt >> result.txt\n"
		case 1:
			script += "printf '%s' \"$2\" >> result.txt\n"
		case 2:
			script += "printf '%s\\n' \"$VALUE\" >&2\n"
		case 3:
			script += "printf '%s' \"$1\"\n"
		case 4:
			script += "printf '\\000\\001\\377' >> result.txt\n"
		}
	}
	script += "cat result.txt > tree/nested/copy\nprintf '' > tree/zero\n"
	if r.Intn(2) == 0 {
		script += "chmod 755 result.txt tree/nested/copy\n"
	}
	// Randomize declarations independently of files produced; undeclared files must be omitted.
	declaration := r.Intn(4)
	if declaration == 1 {
		outputs = []string{"missing", "tree"}
	}
	if declaration == 2 {
		outputs = []string{"result.txt"}
	}
	if declaration == 3 {
		outputs = []string{"missing", "tree/nested/copy", "tree/zero"}
	}
	if index%5 == 4 {
		script += fmt.Sprintf("exit %d\n", 1+r.Intn(3))
	}
	cmd := &repb.Command{
		Arguments:            []string{"/bin/sh", "-c", script, "rand-exec", token(r), token(r)},
		EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "LC_ALL", Value: "C"}, {Name: "PATH", Value: "/usr/bin:/bin"}, {Name: "VALUE", Value: token(r)}},
		OutputPaths:          outputs,
		Platform:             &repb.Platform{Properties: []*repb.Platform_Property{{Name: "Arch", Value: "amd64"}, {Name: "OSFamily", Value: "linux"}, {Name: "workload-isolation-type", Value: "none"}}},
	}
	// Exercise both v2.1 output_paths and the older file/directory declarations.
	if r.Intn(2) == 0 {
		cmd.OutputPaths = nil
		for _, p := range outputs {
			if p == "tree" {
				cmd.OutputDirectories = append(cmd.OutputDirectories, p)
			} else {
				cmd.OutputFiles = append(cmd.OutputFiles, p)
			}
		}
	}
	return testCase{Command: cmd, Input: token(r) + "\n"}
}

func emptySnapshot() snapshot {
	return snapshot{Files: map[string]file{}, Dirs: map[string]bool{}, Links: map[string]string{}}
}
func reference(t *testing.T, ctx context.Context, tc testCase) snapshot {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "input.txt"), []byte(tc.Input), 0644))
	paths := append(append(append([]string{}, tc.Command.OutputPaths...), tc.Command.OutputFiles...), tc.Command.OutputDirectories...)
	for _, p := range paths {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0755))
	}
	cmd := exec.CommandContext(ctx, tc.Command.Arguments[0], tc.Command.Arguments[1:]...)
	cmd.Dir = dir
	for _, v := range tc.Command.EnvironmentVariables {
		cmd.Env = append(cmd.Env, v.Name+"="+v.Value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	s := emptySnapshot()
	if err != nil {
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		s.Exit = int32(exit.ExitCode())
	}
	s.Stdout = stdout.String()
	s.Stderr = stderr.String()
	for _, p := range paths {
		_, err := os.Lstat(filepath.Join(dir, p))
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		require.NoError(t, filepath.Walk(filepath.Join(dir, p), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				s.Links[rel] = target
				return nil
			}
			if info.IsDir() {
				s.Dirs[rel] = true
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			s.Files[rel] = file{string(b), info.Mode()&0111 != 0}
			return nil
		}))
	}
	return s
}

func execute(t *testing.T, ctx context.Context, c *harness.Clients, instance string, d *repb.Digest, skip bool) *repb.ExecuteResponse {
	t.Helper()
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: skip, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	for {
		op, err := stream.Recv()
		require.NotEqual(t, io.EOF, err, "execution ended without completion")
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError(), "operation failed: %v", op.GetError())
		response := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(response))
		require.Zero(t, response.GetStatus().GetCode(), "execution status: %v", response.GetStatus())
		require.NotNil(t, response.Result)
		return response
	}
}

func normalized(t *testing.T, ctx context.Context, c *harness.Clients, instance string, result *repb.ActionResult) snapshot {
	t.Helper()
	s := emptySnapshot()
	s.Exit = result.ExitCode
	read := func(d *repb.Digest) []byte {
		b, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		require.True(t, proto.Equal(harness.Digest(b), d), "CAS digest mismatch")
		return b
	}
	s.Stdout = string(result.StdoutRaw)
	if result.StdoutDigest != nil {
		s.Stdout = string(read(result.StdoutDigest))
	}
	s.Stderr = string(result.StderrRaw)
	if result.StderrDigest != nil {
		s.Stderr = string(read(result.StderrDigest))
	}
	addFile := func(path string, d *repb.Digest, x bool) {
		_, exists := s.Files[path]
		if exists {
			require.Equal(t, s.Files[path], file{string(read(d)), x}, "inconsistent overlapping output %q", path)
			return
		}
		s.Files[path] = file{string(read(d)), x}
	}
	for _, f := range result.OutputFiles {
		addFile(f.Path, f.Digest, f.IsExecutable)
	}
	for _, links := range [][]*repb.OutputSymlink{result.OutputFileSymlinks, result.OutputDirectorySymlinks, result.OutputSymlinks} {
		for _, link := range links {
			s.Links[link.Path] = link.Target
		}
	}
	for _, out := range result.OutputDirectories {
		tree := &repb.Tree{}
		require.NoError(t, proto.Unmarshal(read(out.TreeDigest), tree))
		require.NotNil(t, tree.Root)
		children := map[string]*repb.Directory{}
		for _, child := range tree.Children {
			_, d, err := harness.MarshalDigest(child)
			require.NoError(t, err)
			children[d.Hash] = child
		}
		var walk func(string, *repb.Directory, int)
		walk = func(path string, dir *repb.Directory, depth int) {
			require.Less(t, depth, 32, "cyclic or excessively deep Tree")
			require.NotNil(t, dir, "missing Tree child at %s", path)
			s.Dirs[path] = true
			for _, link := range dir.Symlinks {
				s.Links[path+"/"+link.Name] = link.Target
			}
			for _, f := range dir.Files {
				addFile(path+"/"+f.Name, f.Digest, f.IsExecutable)
			}
			for _, d := range dir.Directories {
				walk(path+"/"+d.Name, children[d.Digest.Hash], depth+1)
			}
		}
		walk(out.Path, tree.Root, 0)
	}
	return s
}

func TestRandomExecutionProperties(t *testing.T) {
	require.Greater(t, *iterations, 0)
	t.Logf("replay: -rand-exec.seed=%d -rand-exec.iterations=%d", *seed, *iterations)
	f := bareFixture(t)
	c := f.Clients(harness.App)
	suiteCtx, suiteCancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer suiteCancel()
	r := rand.New(rand.NewSource(*seed))
	for i := 0; i < *iterations; i++ {
		tc := generate(r, i)
		t.Run(fmt.Sprintf("seed_%d_case_%02d", *seed, i), func(t *testing.T) {
			t.Logf("seed=%d iteration=%d input=%q command=%s", *seed, i, tc.Input, tc.Command)
			ctx, cancel := context.WithTimeout(suiteCtx, 90*time.Second)
			defer cancel()
			expected := reference(t, ctx, tc)
			instance := "rand-exec"
			input, err := c.Upload(ctx, instance, []byte(tc.Input))
			require.NoError(t, err)
			root, err := c.UploadProto(ctx, instance, &repb.Directory{Files: []*repb.FileNode{{Name: "input.txt", Digest: input}}})
			require.NoError(t, err)
			command, err := c.UploadProto(ctx, instance, tc.Command)
			require.NoError(t, err)
			action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: command, InputRootDigest: root, Timeout: durationpb.New(20 * time.Second)})
			require.NoError(t, err)
			first := execute(t, ctx, c, instance, action, true)
			require.False(t, first.CachedResult)
			a := normalized(t, ctx, c, instance, first.Result)
			require.Equal(t, expected, a, "reference vs first fresh")
			if expected.Exit == 0 {
				// Check the AC directly too, so a cache-disabled execution cannot masquerade as coverage.
				cached, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				require.Equal(t, a, normalized(t, ctx, c, instance, cached), "AC vs fresh")
				hit := execute(t, ctx, c, instance, action, false)
				require.True(t, hit.CachedResult, "expected a cache hit after successful cacheable execution")
				require.Equal(t, a, normalized(t, ctx, c, instance, hit.Result), "cached execution vs fresh")
			}
			second := execute(t, ctx, c, instance, action, true)
			require.False(t, second.CachedResult)
			require.Equal(t, a, normalized(t, ctx, c, instance, second.Result), "repeat fresh execution")
		})
		if t.Failed() {
			break
		} // Preserve the first replayable counterexample and bound failures.
	}
}

// Keep declarations canonical if the vocabulary is extended.
func TestGeneratorCanonical(t *testing.T) {
	r := rand.New(rand.NewSource(*seed))
	for i := 0; i < 1000; i++ {
		tc := generate(r, i)
		require.True(t, sort.StringsAreSorted(tc.Command.OutputPaths))
		require.True(t, sort.StringsAreSorted(tc.Command.OutputFiles))
		require.True(t, sort.StringsAreSorted(tc.Command.OutputDirectories))
		for j := 1; j < len(tc.Command.EnvironmentVariables); j++ {
			require.Less(t, strings.Compare(tc.Command.EnvironmentVariables[j-1].Name, tc.Command.EnvironmentVariables[j].Name), 0)
		}
	}
}
