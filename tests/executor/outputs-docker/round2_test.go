package outputs_test

import (
	"fmt"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestConcurrentOutputUploads(t *testing.T) {
	r := start(t)
	// Parallel actions deliberately reuse output paths but have distinct bytes.
	for i := 0; i < 4; i++ {
		i := i
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			marker := fmt.Sprintf("action-%d", i)
			cmd := shell(fmt.Sprintf("mkdir tree; printf %s > tree/marker; dd if=/dev/zero of=large bs=1048576 count=12 2>/dev/null; printf %s >> large", marker, marker))
			cmd.OutputPaths = []string{"large", "tree"}
			result := success(t, r.execute(t, cmd, nil))
			require.Len(t, result.OutputFiles, 1)
			data := r.read(t, result.OutputFiles[0].Digest)
			require.Len(t, data, (12<<20)+len(marker))
			require.Equal(t, marker, string(data[12<<20:]))
			require.Equal(t, make([]byte, 12<<20), data[:12<<20])
			require.Len(t, result.OutputDirectories, 1)
			tree := &repb.Tree{}
			require.NoError(t, proto.Unmarshal(r.read(t, result.OutputDirectories[0].TreeDigest), tree))
			require.Len(t, tree.Root.Files, 1)
			require.Equal(t, marker, string(r.read(t, tree.Root.Files[0].Digest)))
		})
	}
}

func TestChangingOutputIntegrity(t *testing.T) {
	r := start(t)
	for attempt := 0; attempt < 3; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			// The detached writer has bounded lifetime and redirected stdio. No final
			// byte sequence is required; a successful result must reference valid CAS.
			cmd := shell("dd if=/dev/zero of=changing bs=1048576 count=80 2>/dev/null; (end=$(( $(date +%s) + 5 )); while [ $(date +%s) -lt $end ]; do dd if=/dev/urandom of=changing bs=1048576 count=8 conv=notrunc 2>/dev/null; done) </dev/null >/dev/null 2>&1 &")
			cmd.OutputPaths = []string{"changing"}
			response := r.execute(t, cmd, nil)
			if response.GetStatus().GetCode() != 0 {
				t.Logf("changing output rejected: %v", response.Status)
				return
			}
			result := success(t, response)
			require.Len(t, result.OutputFiles, 1)
			r.read(t, result.OutputFiles[0].Digest)
		})
	}
}
