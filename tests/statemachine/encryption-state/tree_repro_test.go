package encryption_test

import (
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// REv2 GetTree documents NOT_FOUND when its root is absent. A valid digest
// naming an absent Directory is a normal cache-miss request, not malformed data.
func TestMissingTreeStatus(t *testing.T) {
	s := startSuite(t, false)
	data, err := proto.Marshal(&repb.Directory{Files: []*repb.FileNode{{Name: "file", Digest: harness.Digest([]byte("file"))}}})
	require.NoError(t, err)
	d := harness.Digest(data)
	var observed []codes.Code
	for sample := 0; sample < 3; sample++ {
		for i, c := range s.clients {
			_, err := c.Read(s.ctx, "", d)
			require.Equal(t, codes.NotFound, status.Code(err))
			stream, err := c.CAS.GetTree(s.ctx, &repb.GetTreeRequest{RootDigest: d})
			require.NoError(t, err)
			_, err = stream.Recv()
			t.Logf("sample=%d client=%d ByteStream=NotFound GetTree=%v", sample, i, err)
			observed = append(observed, status.Code(err))
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, code := range observed {
		require.Equal(t, codes.NotFound, code, "missing tree root must use the REv2 documented status")
	}
}
