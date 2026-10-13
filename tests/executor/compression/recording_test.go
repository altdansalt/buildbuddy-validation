package compression_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// This transparent test-local bridge records executor requests without changing
// protobuf payloads or credentials. Its upstream is always a fixture endpoint.
type recordingBridge struct {
	mu     sync.Mutex
	conn   *grpc.ClientConn
	counts map[string]int
}
type rawCodec struct{}

func (rawCodec) Name() string                  { return "proto" }
func (rawCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (rawCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}

func startBridge(t *testing.T) (*recordingBridge, string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &recordingBridge{counts: map[string]int{}}
	server := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(b.forward), grpc.MaxRecvMsgSize(64<<20), grpc.MaxSendMsgSize(64<<20))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	// Use a numeric runner address: the isolated harness daemon has no
	// default docker0 bridge, so Docker host-gateway is not portable here.
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		if strings.HasPrefix(iface.Name, "br-") || strings.HasPrefix(iface.Name, "veth") || iface.Name == "docker0" {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			t.Fatal(err)
		}
		for _, addr := range addresses {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil {
				return b, net.JoinHostPort(ip.String(), fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
			}
		}
	}
	t.Fatal("no non-loopback runner IPv4 address for fixture-local recording bridge")
	return nil, ""
}
func (b *recordingBridge) forward(_ any, downstream grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(downstream)
	md, _ := metadata.FromIncomingContext(downstream.Context())
	ctx, cancel := context.WithCancel(metadata.NewOutgoingContext(downstream.Context(), md.Copy()))
	defer cancel()
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("recording bridge upstream not initialized")
	}
	upstream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method, grpc.ForceCodec(rawCodec{}))
	if err != nil {
		return err
	}
	sent := make(chan error, 1)
	go func() {
		for {
			var data []byte
			err := downstream.RecvMsg(&data)
			if err == io.EOF {
				sent <- upstream.CloseSend()
				return
			}
			if err != nil {
				sent <- err
				cancel()
				return
			}
			b.record(method, data)
			if err := upstream.SendMsg(&data); err != nil {
				sent <- err
				return
			}
		}
	}()
	headers, err := upstream.Header()
	if err != nil {
		return err
	}
	if err := downstream.SendHeader(headers); err != nil {
		return err
	}
	for {
		var data []byte
		err := upstream.RecvMsg(&data)
		if err == io.EOF {
			downstream.SetTrailer(upstream.Trailer())
			return <-sent
		}
		if err != nil {
			downstream.SetTrailer(upstream.Trailer())
			return err
		}
		if err := downstream.SendMsg(&data); err != nil {
			return err
		}
	}
}
func (b *recordingBridge) record(method string, data []byte) {
	keys := []string{}
	switch {
	case strings.HasSuffix(method, "/Read"):
		req := &bspb.ReadRequest{}
		if proto.Unmarshal(data, req) == nil {
			keys = append(keys, "read/"+encodingName(req.ResourceName))
		}
	case strings.HasSuffix(method, "/Write"):
		req := &bspb.WriteRequest{}
		if proto.Unmarshal(data, req) == nil && req.ResourceName != "" {
			keys = append(keys, "write/"+encodingName(req.ResourceName))
		}
	case strings.HasSuffix(method, "/BatchUpdateBlobs"):
		req := &repb.BatchUpdateBlobsRequest{}
		if proto.Unmarshal(data, req) == nil {
			for _, r := range req.Requests {
				keys = append(keys, "batch-upload/"+r.Compressor.String())
			}
		}
	case strings.HasSuffix(method, "/BatchReadBlobs"):
		req := &repb.BatchReadBlobsRequest{}
		if proto.Unmarshal(data, req) == nil {
			key := "batch-read/identity"
			if hasZstd(req.AcceptableCompressors) {
				key = "batch-read/zstd-accepted"
			}
			keys = append(keys, key)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range keys {
		b.counts[k]++
	}
}
func encodingName(name string) string {
	if strings.Contains(name, "/compressed-blobs/zstd/") || strings.HasPrefix(name, "compressed-blobs/zstd/") {
		return "zstd"
	}
	return "identity"
}
func (b *recordingBridge) log(t *testing.T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t.Logf("seed=%d executor wire requests: %v", *seed, b.counts)
}

func (b *recordingBridge) check(t *testing.T, upload bool) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.counts["read/zstd"] == 0 || b.counts["batch-read/zstd-accepted"] == 0 {
		t.Errorf("seed=%d expected streaming and batch compressed input requests: %v", *seed, b.counts)
	}
	compressedUploads := b.counts["write/zstd"] + b.counts["batch-upload/ZSTD"]
	if upload && compressedUploads == 0 {
		t.Errorf("seed=%d expected compressed output uploads: %v", *seed, b.counts)
	}
	if !upload && compressedUploads != 0 {
		t.Errorf("seed=%d unexpected compressed output uploads with upload compression disabled: %v", *seed, b.counts)
	}
}
