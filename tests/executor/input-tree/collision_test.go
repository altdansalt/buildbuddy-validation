package inputtree_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

// The released materializer fails on colliding file/directory names. REAPI permits
// servers to skip canonicalization checks; INVALID_ARGUMENT is not required.
func TestFileDirectoryCollision(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	blob, err := c.Upload(ctx, "input-tree", []byte("collision"))
	require.NoError(t, err)
	empty, err := c.UploadProto(ctx, "input-tree", &repb.Directory{})
	require.NoError(t, err)
	root := &repb.Directory{Files: []*repb.FileNode{{Name: "same", Digest: blob}}, Directories: []*repb.DirectoryNode{{Name: "same", Digest: empty}}}
	r := execute(t, c, root, "echo invalid-tree-was-executed")
	require.EqualValues(t, 2, r.GetStatus().GetCode(), "released materializer reports UNKNOWN, got %v", r.GetStatus())
	require.Contains(t, r.GetStatus().GetMessage(), "same: is a directory")
	require.Empty(t, r.GetResult().GetStdoutRaw())
	require.Nil(t, r.GetResult().GetStdoutDigest())
}
