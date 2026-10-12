package bytestream_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestReadBeyondEOF(t *testing.T) {
	proxy, app, ctx := fixture(t)
	for _, warm := range []bool{false, true} {
		data := []byte("offset must not exceed the resource size")
		if warm {
			data = append(data, '!')
		}
		d, err := app.Write(ctx, "beyond-eof", data, false)
		require.NoError(t, err)
		if warm {
			_, err = proxy.Read(ctx, "beyond-eof", d)
			require.NoError(t, err)
		}
		got, err := read(ctx, proxy, harness.ResourceName("beyond-eof", d, false), int64(len(data))+1, 0)
		require.Equal(t, codes.OutOfRange, status.Code(err), "warm=%t got=%d bytes err=%v", warm, len(got), err)
	}
}
