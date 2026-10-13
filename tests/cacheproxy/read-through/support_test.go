package readthrough_test

import (
	"context"
	"io"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/metadata"
)

const instance = "read-through"

func local(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "proxy_skip_remote", "true")
}
func read(ctx context.Context, c *harness.Clients, d *repb.Digest, offset, limit int64, compressed bool) ([]byte, error) {
	s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, compressed), ReadOffset: offset, ReadLimit: limit})
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
			return b, err
		}
		b = append(b, r.Data...)
	}
}
