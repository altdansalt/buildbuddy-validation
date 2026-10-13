package encryption_test

import (
	"fmt"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestEncryptionState(t *testing.T) {
	s := startSuite(t, true)
	for step, enabled := range []bool{false, true, true, false, false, true, true} {
		s.config(enabled, 15*time.Second)
		s.write(2*step, step%2 == 0, false)
		s.write(2*step+1, step%2 != 0, true)
		s.write(1000+step, step%2 == 0, true)
		file := s.blobs[len(s.blobs)-3]
		directory := s.blobs[len(s.blobs)-1]
		command := s.putProto(&repb.Command{Arguments: []string{"true"}, EnvironmentVariables: []*repb.Command_EnvironmentVariable{{Name: "STEP", Value: fmt.Sprint(step)}}, OutputPaths: []string{"output", "link", "dir", "dirlink"}}, step%4)
		action := s.putProto(&repb.Action{CommandDigest: command, InputRootDigest: directory.d}, (step+1)%4)
		tree := s.putProto(&repb.Tree{Root: directory.dir, Children: []*repb.Directory{s.blobs[len(s.blobs)-4].dir}}, (step+2)%4)
		for update := 0; update < 3; update++ {
			want := &repb.ActionResult{ExitCode: int32(update), StdoutRaw: []byte(fmt.Sprintf("stdout-%d-%d", step, update)), StderrRaw: []byte("stderr"), OutputFiles: []*repb.OutputFile{{Path: "output", Digest: file.d, IsExecutable: true}}, OutputFileSymlinks: []*repb.OutputSymlink{{Path: "link", Target: "output"}}, OutputDirectories: []*repb.OutputDirectory{{Path: "dir", TreeDigest: tree}}, OutputDirectorySymlinks: []*repb.OutputSymlink{{Path: "dirlink", Target: "dir"}}}
			for repeat := 0; repeat < 2; repeat++ {
				got, err := s.clients[(update+repeat)%4].AC.UpdateActionResult(s.ctx, &repb.UpdateActionResultRequest{ActionDigest: action, ActionResult: want})
				require.NoError(t, err)
				// Only server-assigned worker metadata is normalized; all submitted fields
				// and the returned result must agree on every read surface.
				normalized := proto.Clone(got).(*repb.ActionResult)
				normalized.ExecutionMetadata = nil
				require.True(t, proto.Equal(want, normalized))
				record := actionRecord{d: action, result: got, namespace: s.namespace}
				found := false
				for i := range s.actions {
					if proto.Equal(s.actions[i].d, action) && s.actions[i].namespace == s.namespace {
						s.actions[i] = record
						found = true
					}
				}
				if !found {
					s.actions = append(s.actions, record)
				}
				s.snapshot()
			}
		}
	}
}
