package largeblobs_test

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

// REAPI requires INVALID_ARGUMENT for nonzero compressed read limits.
func TestCompressedReadLimit(t *testing.T) {
	f := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	data, d := blob(t, 64<<20, 99)
	_, err := upload(ctx, f.Clients(harness.App), harness.UploadResourceName(instance, d, false), data, 0)
	require.NoError(t, err)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			s, e := f.Clients(component).ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, true), ReadOffset: d.SizeBytes - 17, ReadLimit: 13})
			if e == nil {
				for {
					_, e = s.Recv()
					if e != nil {
						break
					}
				}
				if e == io.EOF {
					e = nil
				}
			}
			t.Logf("compressed read with limit: %v", e)
			require.Equal(t, codes.InvalidArgument, status.Code(e))
		})
	}
}
