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
	for _, state := range []string{"cold", "warm"} {
		t.Run(state, func(t *testing.T) {
			data := []byte("offset must not exceed the resource size: " + state)
			d, err := app.Write(ctx, "beyond-eof", data, false)
			require.NoError(t, err)
			name := harness.ResourceName("beyond-eof", d, false)
			if state == "warm" {
				got, err := proxy.Read(ctx, "beyond-eof", d)
				require.NoError(t, err)
				require.Equal(t, data, got)
			}
			for label, c := range map[string]*harness.Clients{"proxy": proxy, "app": app} {
				t.Run(label, func(t *testing.T) {
					got, err := read(ctx, c, name, int64(len(data))+1, 0)
					require.Equal(t, codes.OutOfRange, status.Code(err), "got=%d bytes err=%v", len(got), err)
				})
			}
			// Recovery after the malformed request must preserve the blob.
			got, err := proxy.Read(ctx, "beyond-eof", d)
			require.NoError(t, err)
			require.Equal(t, data, got)
		})
	}
}
