package compressedrange_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/emptypb"
)

func request(uri string, end uint64) *emptypb.Empty {
	b := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte(uri))
	b = protowire.AppendVarint(protowire.AppendTag(b, 3, protowire.VarintType), end)
	m := &emptypb.Empty{}
	m.ProtoReflect().SetUnknown(b)
	return m
}
func responseData(t *testing.T, m *emptypb.Empty) []byte {
	b := m.ProtoReflect().GetUnknown()
	n, typ, k := protowire.ConsumeTag(b)
	require.Equal(t, protowire.Number(1), n)
	require.Equal(t, protowire.BytesType, typ)
	require.Greater(t, k, 0)
	data, k := protowire.ConsumeBytes(b[k:])
	require.Greater(t, k, 0)
	return data
}

// proto/api/v1/file.proto explicitly permits compressed GetFileRange URIs.
// A full range (start=0,end=digest_size) is valid and under the 16 MiB limit.
func TestCompressedGetFileRange(t *testing.T) {
	for _, mutation := range []string{"BatchUpdateBlobs", "ByteStreamWrite"} {
		t.Run(mutation, func(t *testing.T) { testCompressedGetFileRange(t, mutation) })
	}
}

func testCompressedGetFileRange(t *testing.T, mutation string) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	expected := bytes.Repeat([]byte("range-conformance\n"), 32)
	d := harness.Digest(expected)
	var err error
	if mutation == "BatchUpdateBlobs" {
		d, err = c.Upload(ctx, "", expected)
	} else {
		d, err = c.Write(ctx, "", expected, true)
	}
	require.NoError(t, err)
	identity := "bytestream://app:1985/" + harness.ResourceName("", d, false)
	compressed := "bytestream://app:1985/" + harness.ResourceName("", d, true)
	rsp := &emptypb.Empty{}
	require.NoError(t, c.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", request(identity, uint64(d.SizeBytes)), rsp))
	require.Equal(t, expected, responseData(t, rsp))
	stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName("", d, true)})
	require.NoError(t, err)
	var encoded []byte
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		encoded = append(encoded, r.Data...)
	}
	decoded, err := harness.Decompress(encoded)
	require.NoError(t, err)
	require.Equal(t, expected, decoded)
	started := time.Now()
	deadline := started.Add(15 * time.Second)
	var last error
	attempts := 0
	for {
		rsp = &emptypb.Empty{}
		last = c.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", request(compressed, uint64(d.SizeBytes)), rsp)
		attempts++
		if last == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("compressed GetFileRange after successful %s: attempts=%d observation=%s last=%v", mutation, attempts, time.Since(started), last)
	payload, _ := json.Marshal(map[string]any{"uri": compressed, "start": "0", "end": strconv.FormatInt(d.SizeBytes, 10)})
	req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/GetFileRange", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	resp, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	t.Logf("HTTP compressed GetFileRange: status=%d body=%s", resp.StatusCode, body)
	if last != nil {
		t.Errorf("documented compressed URI must return a compressed range: %v", last)
	} else {
		decoded, err := harness.Decompress(responseData(t, rsp))
		require.NoError(t, err)
		require.Equal(t, expected, decoded)
	}
	require.Equal(t, http.StatusOK, resp.StatusCode, fmt.Sprint(string(body)))
}
