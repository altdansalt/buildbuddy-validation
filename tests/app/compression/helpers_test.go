package compression

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"io"
	"testing"
	"time"
)

func fixture(t *testing.T) (context.Context, *harness.Clients) {
	t.Helper()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	c := f.Clients(harness.App)
	caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.Contains(t, caps.GetCacheCapabilities().GetSupportedCompressors(), repb.Compressor_ZSTD)
	require.Contains(t, caps.GetCacheCapabilities().GetSupportedBatchUpdateCompressors(), repb.Compressor_ZSTD)
	return ctx, c
}

func read(ctx context.Context, c *harness.Clients, req *bspb.ReadRequest) ([]byte, error) {
	s, err := c.ByteStream.Read(ctx, req)
	if err != nil {
		return nil, err
	}
	var b []byte
	for {
		r, err := s.Recv()
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return nil, err
		}
		b = append(b, r.Data...)
	}
}

func write(ctx context.Context, c *harness.Clients, name string, b []byte) (*bspb.WriteResponse, error) {
	s, err := c.ByteStream.Write(ctx)
	if err != nil {
		return nil, err
	}
	// Small chunks exercise offsets measured in compressed bytes, including frame splits.
	chunkSize := 7
	if len(b) > 10000 {
		chunkSize = 64 << 10
	}
	for off := 0; ; {
		end := off + chunkSize
		if end > len(b) {
			end = len(b)
		}
		err = s.Send(&bspb.WriteRequest{ResourceName: name, WriteOffset: int64(off), Data: b[off:end], FinishWrite: end == len(b)})
		if err != nil {
			break
		}
		off = end
		if end == len(b) {
			break
		}
	}
	return s.CloseAndRecv()
}
