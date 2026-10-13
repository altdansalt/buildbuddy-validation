package encryption_test

import (
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// AC values are readable at both endpoints, but proxy metadata uses a different
// internal key. No shortened authentication/key TTLs or enabled encryption.
func TestActionMetadataAgreement(t *testing.T) {
	s := startSuite(t, false)
	command, err := s.clients[0].UploadProto(s.ctx, "", &repb.Command{Arguments: []string{"true"}})
	require.NoError(t, err)
	root, err := s.clients[0].UploadProto(s.ctx, "", &repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "target"}}})
	require.NoError(t, err)
	d, err := s.clients[0].UploadProto(s.ctx, "", &repb.Action{CommandDigest: command, InputRootDigest: root})
	require.NoError(t, err)
	want := &repb.ActionResult{StdoutRaw: []byte("metadata-result"), StderrRaw: []byte("stderr"), ExitCode: 3}
	got, err := s.clients[1].AC.UpdateActionResult(s.ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: want})
	require.NoError(t, err)
	for _, c := range s.clients {
		read, err := c.AC.GetActionResult(s.ctx, &repb.GetActionResultRequest{ActionDigest: d})
		require.NoError(t, err)
		require.True(t, proto.Equal(got, read))
	}
	dw, err := proto.Marshal(d)
	require.NoError(t, err)
	req := msg(2, join(msg(1, dw), num(4, 1)))
	appMD, err := s.invoke(s.clients[0], "/cache.service.Cache/GetMetadata", req)
	require.NoError(t, err)
	_, logical := field(appMD, 5)
	require.Equal(t, uint64(d.SizeBytes), logical)
	start := time.Now()
	var proxyMD []byte
	var lastErr error
	deadline := start.Add(15 * time.Second)
	for {
		proxyMD, lastErr = s.invoke(s.clients[1], "/cache.service.Cache/GetMetadata", req)
		if lastErr == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("app AC metadata exists; proxy metadata after %s: %v", time.Since(start), lastErr)
	require.NoError(t, lastErr, "proxy AC value is readable but its metadata never becomes visible")
	_, proxyLogical := field(proxyMD, 5)
	require.Equal(t, logical, proxyLogical)
}
