package bytestream_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestCompletedWriteStatus(t *testing.T) {
	proxy, app, ctx := fixture(t)
	data := []byte("completed upload remains readable without status queries")
	d := harness.Digest(data)
	name := harness.UploadResourceName("status", d, false)
	stream, err := proxy.ByteStream.Write(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&bspb.WriteRequest{ResourceName: name, Data: data, FinishWrite: true}))
	response, err := stream.CloseAndRecv()
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), response.CommittedSize)
	got, err := app.Read(ctx, "status", d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	for label, c := range map[string]*harness.Clients{"proxy": proxy, "app": app} {
		t.Run(label, func(t *testing.T) {
			_, err := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: name})
			// The app deliberately does not implement status queries; the proxy forwards this.
			require.Equal(t, codes.Unimplemented, status.Code(err))
			got, err := c.Read(ctx, "status", d)
			require.NoError(t, err)
			require.Equal(t, data, got)
		})
	}
}
