package bytestream_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"testing"
)

func TestCompletedWriteStatus(t *testing.T) {
	proxy, app, ctx := fixture(t)
	data := []byte("completed upload has a queryable committed size")
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
			result, err := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: name})
			require.NoError(t, err, "completed upload QueryWriteStatus")
			require.True(t, result.Complete)
			require.Equal(t, int64(len(data)), result.CommittedSize)
		})
	}
}
