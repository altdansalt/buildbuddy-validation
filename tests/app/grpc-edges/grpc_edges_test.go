package grpcedges

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const method = "/build.bazel.remote.execution.v2.Capabilities/GetCapabilities"

// rawCodec sends exact wire bytes while retaining the standard proto content type.
type rawCodec struct{}

func (rawCodec) Name() string                    { return "proto" }
func (rawCodec) Marshal(v any) ([]byte, error)   { return v.([]byte), nil }
func (rawCodec) Unmarshal(b []byte, v any) error { return proto.Unmarshal(b, v.(proto.Message)) }

func bounded() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}
func healthy(t *testing.T, c *harness.Clients) {
	t.Helper()
	ctx, cancel := bounded()
	defer cancel()
	r, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	if err != nil || r.GetCacheCapabilities() == nil {
		t.Fatalf("capabilities after edge request: %v, %v", r, err)
	}
}

func TestGRPCEdges(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	healthy(t, c)
	t.Run("unknown_fields", func(t *testing.T) {
		wire := protowire.AppendTag(nil, 19000, protowire.BytesType)
		wire = protowire.AppendBytes(wire, []byte("future extension"))
		ctx, cancel := bounded()
		defer cancel()
		var response repb.ServerCapabilities
		if err := c.Conn.Invoke(ctx, method, wire, &response, grpc.ForceCodec(rawCodec{})); err != nil {
			t.Fatal(err)
		}
		if response.GetCacheCapabilities() == nil {
			t.Fatal("missing capabilities")
		}
	})
	t.Run("malformed_protos", func(t *testing.T) {
		// Invalid tag zero, truncated length, and illegal wire type.
		for _, wire := range [][]byte{{0}, {0x0a, 0x80}, {0x0f}} {
			ctx, cancel := bounded()
			err := c.Conn.Invoke(ctx, method, wire, &repb.ServerCapabilities{}, grpc.ForceCodec(rawCodec{}))
			cancel()
			// gRPC's status table specifies INTERNAL for a request proto parsing failure.
			if status.Code(err) != codes.Internal {
				t.Errorf("wire %x: got %v, want INTERNAL", wire, err)
			}
			healthy(t, c)
		}
	})
	t.Run("receive_size_boundary", func(t *testing.T) {
		// The release defaults to 50,000,000 bytes. Unknown fields isolate transport
		// enforcement from CAS batch limits and application-level string validation.
		for _, size := range []int{50_000_000, 50_000_001} {
			wire := protowire.AppendTag(nil, 19000, protowire.BytesType)
			wire = protowire.AppendBytes(wire, make([]byte, size-7))
			if len(wire) != size {
				t.Fatalf("wire length %d != %d", len(wire), size)
			}
			ctx, cancel := bounded()
			err := c.Conn.Invoke(ctx, method, wire, &repb.ServerCapabilities{}, grpc.ForceCodec(rawCodec{}))
			cancel()
			want := codes.OK
			if size > 50_000_000 {
				want = codes.ResourceExhausted
			}
			if status.Code(err) != want {
				t.Errorf("%d bytes: got %v, want %v", size, err, want)
			}
			healthy(t, c)
		}
	})
	t.Run("metadata", func(t *testing.T) {
		ctx, cancel := bounded()
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "x-edge-test", "first", "x-edge-test", "second", "x-edge-bin", string([]byte{0, 0xff, 1}))
		if _, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("deadlines_and_multiplexing", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ctx, cancel := bounded()
				defer cancel()
				want := codes.OK
				if i%2 == 0 {
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
					want = codes.DeadlineExceeded
				}
				_, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
				if status.Code(err) != want {
					t.Errorf("RPC %d: %v, want %v", i, err, want)
				}
			}(i)
		}
		wg.Wait()
		healthy(t, c)
	})
	t.Run("http2_wire", func(t *testing.T) {
		tr := &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: 20 * time.Second}
		for _, tc := range []struct {
			name, contentType, timeout string
			frame                      []byte
			httpStatus                 int
			grpcStatus                 string
		}{
			{"proto", "application/grpc+proto", "", []byte{0, 0, 0, 0, 0}, 200, "0"},
			{"bare", "application/grpc", "", []byte{0, 0, 0, 0, 0}, 200, "0"},
			{"wrong_content_type", "application/json", "", []byte{0, 0, 0, 0, 0}, 415, "3"},
			{"expired_deadline", "application/grpc", "1n", []byte{0, 0, 0, 0, 0}, 200, "4"},
			{"invalid_timeout", "application/grpc", "garbage", []byte{0, 0, 0, 0, 0}, 400, "13"},
			{"unsupported_compression", "application/grpc", "", []byte{1, 0, 0, 0, 0}, 200, "12"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ctx, cancel := bounded()
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, "POST", "http://"+f.Endpoint(harness.App).GRPC+method, bytes.NewReader(tc.frame))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", tc.contentType)
				req.Header.Set("TE", "trailers")
				req.Header.Set("x-buildbuddy-api-key", f.APIKey)
				if tc.timeout != "" {
					req.Header.Set("grpc-timeout", tc.timeout)
				}
				if tc.name == "unsupported_compression" {
					req.Header.Set("grpc-encoding", "not-a-codec")
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				got := resp.Trailer.Get("Grpc-Status")
				if got == "" {
					got = resp.Header.Get("Grpc-Status")
				}
				if resp.ProtoMajor != 2 || resp.StatusCode != tc.httpStatus || got != tc.grpcStatus {
					t.Fatalf("HTTP %s %d, grpc-status=%q, body=%q", resp.Proto, resp.StatusCode, got, body)
				}
				if got == "0" {
					if len(body) < 5 || body[0] != 0 || int(binary.BigEndian.Uint32(body[1:5])) != len(body)-5 {
						t.Fatalf("invalid response frame: %x", body)
					}
					var response repb.ServerCapabilities
					if err := proto.Unmarshal(body[5:], &response); err != nil || response.GetCacheCapabilities() == nil {
						t.Fatalf("response: %v, %v", &response, err)
					}
				}
			})
		}
		healthy(t, c)
	})
}
