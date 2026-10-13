package membership

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
)

// Minimal valid protobuf wire messages keep this area independent of the
// BuildBuddy source tree. Field numbers are from the released proto files.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), b...)
	return nil
}
func bytesField(n int, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, protowire.Number(n), protowire.BytesType), b)
}
func text(n int, s string) []byte { return bytesField(n, []byte(s)) }
func number(n int, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, protowire.Number(n), protowire.VarintType), v)
}
func join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
func requestContext(uid, gid string) []byte { return join(bytesField(1, text(1, uid)), text(2, gid)) }

type fields map[int][][]byte

func decode(b []byte) (fields, error) {
	out := fields{}
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			return nil, fmt.Errorf("tag: %d", k)
		}
		b = b[k:]
		var val []byte
		switch typ {
		case protowire.BytesType:
			val, k = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			var v uint64
			v, k = protowire.ConsumeVarint(b)
			val = []byte(fmt.Sprint(v))
		default:
			k = protowire.ConsumeFieldValue(n, typ, b)
		}
		if k < 0 {
			return nil, fmt.Errorf("field: %d", k)
		}
		out[int(n)] = append(out[int(n)], val)
		b = b[k:]
	}
	return out, nil
}
func first(f fields, n int) []byte {
	if len(f[n]) == 0 {
		return nil
	}
	return f[n][0]
}
func rpc(conn *grpc.ClientConn, key, method string, req []byte) (fields, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-api-key", key)
	var b []byte
	err := conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, req, &b, grpc.ForceCodec(wireCodec{}))
	if err != nil {
		return nil, err
	}
	return decode(b)
}
