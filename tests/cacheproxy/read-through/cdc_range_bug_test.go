package readthrough_test

import (
	"math/rand"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

func TestCDCBoundedRead(t *testing.T) {
	app, writer, reader, ctx := cdcFixture(t)
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(20261013)).Read(data)
	d, err := writer.Clients(harness.CacheProxy).Write(ctx, instance, data, false)
	require.NoError(t, err)
	for _, c := range []*harness.Clients{app.Clients(harness.App), writer.Clients(harness.CacheProxy), reader.Clients(harness.CacheProxy)} {
		got, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
	// The same valid resource must support a bounded identity ByteStream read.
	got, err := read(ctx, reader.Clients(harness.CacheProxy), d, 12345, 17, false)
	require.NoError(t, err, "CDC full read succeeded but bounded read failed")
	require.Equal(t, data[12345:12362], got)
}
