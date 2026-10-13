package convergence_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"io"
	"net/http"
	"strconv"
	"strings"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"testing"
	"time"
)

// The scaffold has no BuildBuddy proto bindings. These helpers encode the exact
// released proto field numbers; wrappers are wire-compatible with uri/data = 1.
func field(b []byte, n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, n, protowire.BytesType), v)
}
func number(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}
func resource(d *repb.Digest, instance string, ac bool) []byte {
	b, _ := proto.Marshal(d)
	r := field(nil, 1, b)
	r = field(r, 2, []byte(instance))
	typ := uint64(2)
	if ac {
		typ = 1
	}
	return number(r, 4, typ)
}
func wire(b []byte) *emptypb.Empty { m := &emptypb.Empty{}; m.ProtoReflect().SetUnknown(b); return m }

func fields(t *testing.T, b []byte, n protowire.Number) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		num, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			require.Positive(t, k)
			if num == n {
				out = append(out, v)
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(num, typ, b)
			require.Positive(t, k)
			b = b[k:]
		}
	}
	return out
}
func integer(t *testing.T, b []byte, n protowire.Number) uint64 {
	t.Helper()
	for len(b) > 0 {
		num, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if num == n {
			require.Equal(t, protowire.VarintType, typ)
			v, k := protowire.ConsumeVarint(b)
			require.Positive(t, k)
			return v
		}
		k = protowire.ConsumeFieldValue(num, typ, b)
		require.Positive(t, k)
		b = b[k:]
	}
	return 0
}
func checkMetadata(t *testing.T, b []byte, size int64) {
	t.Helper()
	require.Equal(t, uint64(size), integer(t, b, 5))
	require.Positive(t, integer(t, b, 2))
	for _, n := range []protowire.Number{3, 4} {
		v := integer(t, b, n)
		require.Positive(t, v)
		require.LessOrEqual(t, v, uint64(time.Now().Add(time.Minute).UnixMicro()))
	}
}

// This is the exact length-prefixed HTTP protocol used by rpc_service.ts.
func frontendHTTP(t *testing.T, ctx context.Context, f *harness.Fixture, method string, request []byte) ([][]byte, string) {
	t.Helper()
	body := make([]byte, 5)
	binary.BigEndian.PutUint32(body[1:], uint32(len(request)))
	body = append(body, request...)
	req, err := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/rpc/BuildBuddyService/"+method, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/proto+prefixed")
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode, string(data))
	code := res.Header.Get("grpc-status")
	var messages [][]byte
	for len(data) > 0 {
		require.GreaterOrEqual(t, len(data), 5)
		flag := data[0]
		size := int(binary.BigEndian.Uint32(data[1:5]))
		data = data[5:]
		require.GreaterOrEqual(t, len(data), size)
		payload := data[:size]
		data = data[size:]
		if flag == 0x80 {
			for _, line := range strings.Split(string(payload), "\r\n") {
				if strings.HasPrefix(strings.ToLower(line), "grpc-status:") {
					code = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				}
			}
		} else {
			require.Equal(t, byte(0), flag)
			messages = append(messages, payload)
		}
	}
	require.NotEmpty(t, code, fmt.Sprintf("%s missing terminal status", method))
	return messages, code
}

func checkWebMetadata(t *testing.T, m map[string]any, size int64) {
	t.Helper()
	for _, name := range []string{"storedSizeBytes", "digestSizeBytes", "lastAccessUsec", "lastModifyUsec"} {
		v, err := strconv.ParseUint(fmt.Sprint(m[name]), 10, 64)
		require.NoError(t, err)
		require.Positive(t, v)
		if name == "digestSizeBytes" {
			require.Equal(t, uint64(size), v)
		}
		if name == "lastAccessUsec" || name == "lastModifyUsec" {
			require.LessOrEqual(t, v, uint64(time.Now().Add(time.Minute).UnixMicro()))
		}
	}
}
