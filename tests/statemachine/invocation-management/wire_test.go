package invocation_management_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"testing"
	"time"
)

// rawCodec sends valid release-schema protobufs without generated app bindings.
type rawCodec struct{}

func (rawCodec) Name() string                  { return "proto" }
func (rawCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (rawCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func bytesFields(t *testing.T, b []byte, n protowire.Number) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		k, typ, l := protowire.ConsumeTag(b)
		require.Positive(t, l)
		b = b[l:]
		z := protowire.ConsumeFieldValue(k, typ, b)
		require.Positive(t, z)
		if k == n && typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(b)
			out = append(out, v)
		}
		b = b[z:]
	}
	return out
}
func first(t *testing.T, b []byte, n protowire.Number) []byte {
	v := bytesFields(t, b, n)
	if len(v) == 0 {
		return nil
	}
	return v[0]
}
func call(c *harness.Clients, method string, b []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out []byte
	err := c.Conn.Invoke(ctx, method, b, &out, grpc.ForceCodec(rawCodec{}))
	return out, err
}

const ui = "/buildbuddy.service.BuildBuddyService/"
