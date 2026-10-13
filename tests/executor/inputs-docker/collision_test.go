package inputtree_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

// Directory children must have unique names across files, directories and symlinks.
// https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto
func TestFileDirectoryCollision(t *testing.T) {
	f := fixture(t)
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	blob, err := c.Upload(ctx, "input-tree", []byte("collision"))
	require.NoError(t, err)
	empty, err := c.UploadProto(ctx, "input-tree", &repb.Directory{})
	require.NoError(t, err)
	root := &repb.Directory{Files: []*repb.FileNode{{Name: "same", Digest: blob}}, Directories: []*repb.DirectoryNode{{Name: "same", Digest: empty}}}
	r := execute(t, c, root, "echo invalid-tree-was-executed")
	require.NotZero(t, r.GetStatus().GetCode(), "malformed tree must not execute")
	t.Logf("collision status: %v", r.GetStatus())
}
