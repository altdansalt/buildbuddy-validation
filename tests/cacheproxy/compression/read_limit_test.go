package compression_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCompressedReadLimitRejected(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	proxy, app := f.Clients(harness.CacheProxy), f.Clients(harness.App)
	d, err := app.Write(ctx, "limit-repro", []byte("compressed read limit reproducer"), false)
	require.NoError(t, err)
	// Consume the stream: errors may arrive only on Recv, not Read.
	for _, c := range []*harness.Clients{app, proxy} {
		s, e := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName("limit-repro", d, true), ReadLimit: 1})
		var received int
		if e == nil {
			for {
				r, recvErr := s.Recv()
				if recvErr != nil {
					e = recvErr
					break
				}
				received += len(r.Data)
			}
		}
		if e == io.EOF {
			e = nil
		}
		t.Logf("endpoint %s: status=%s compressed bytes=%d", c.Conn.Target(), status.Code(e), received)
		// The app is a fixture-local control. Record its behavior without masking
		// the proxy's conformance assertion if both share the underlying defect.
		if c == proxy {
			require.Equal(t, codes.InvalidArgument, status.Code(e), "compressed reads with nonzero limits must fail")
		}
	}
}
