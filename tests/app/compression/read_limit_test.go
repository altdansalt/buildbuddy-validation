package compression

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

// REAPI explicitly forbids nonzero read_limit for compressed downloads.
func TestCompressedReadRejectsNonzeroLimit(t *testing.T) {
	ctx, c := fixture(t)
	data := []byte("compressed read limit reproduction")
	d, err := c.Upload(ctx, "read-limit", data)
	require.NoError(t, err)
	// Check the identity path and full compressed read before testing the invalid request.
	got, err := c.Read(ctx, "read-limit", d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	b, err := read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName("read-limit", d, true)})
	require.NoError(t, err)
	got, err = harness.Decompress(b)
	require.NoError(t, err)
	require.Equal(t, data, got)
	b, err = read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName("read-limit", d, true), ReadLimit: 1})
	t.Logf("compressed read with limit=1: status=%s wire_bytes=%d error=%v", status.Code(err), len(b), err)
	if err == nil {
		got, decodeErr := harness.Decompress(b)
		t.Logf("decompressed response=%q decode_error=%v", got, decodeErr)
	}
	require.Equal(t, codes.InvalidArgument, status.Code(err), "compressed reads MUST reject a nonzero read_limit")
}
