package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

type Clients struct {
	Conn         *grpc.ClientConn
	CAS          repb.ContentAddressableStorageClient
	ByteStream   bspb.ByteStreamClient
	AC           repb.ActionCacheClient
	Execution    repb.ExecutionClient
	Capabilities repb.CapabilitiesClient
	BES          bespb.PublishBuildEventClient
}

// Dial accepts a test-local endpoint and a test-local key. It forbids cloud
// targets and attaches the key to unary and streaming RPCs unless overridden.
func Dial(t testing.TB, target, key string) *Clients {
	t.Helper()
	if strings.Contains(target, "buildbuddy.io") {
		t.Fatalf("cloud endpoint forbidden: %s", target)
	}
	addKey := func(ctx context.Context) context.Context {
		md, _ := metadata.FromOutgoingContext(ctx)
		if key != "" && len(md.Get("x-buildbuddy-api-key")) == 0 {
			return WithAPIKey(ctx, key)
		}
		return ctx
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(addKey(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(addKey(ctx), desc, cc, method, opts...)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &Clients{Conn: conn, CAS: repb.NewContentAddressableStorageClient(conn), ByteStream: bspb.NewByteStreamClient(conn), AC: repb.NewActionCacheClient(conn), Execution: repb.NewExecutionClient(conn), Capabilities: repb.NewCapabilitiesClient(conn), BES: bespb.NewPublishBuildEventClient(conn)}
}
func (f *Fixture) Clients(c Component) *Clients { return Dial(f.t, f.Endpoint(c).GRPC, f.APIKey) }
func WithAPIKey(ctx context.Context, key string) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set("x-buildbuddy-api-key", key)
	return metadata.NewOutgoingContext(ctx, md)
}
func Digest(b []byte) *repb.Digest {
	h := sha256.Sum256(b)
	return &repb.Digest{Hash: hex.EncodeToString(h[:]), SizeBytes: int64(len(b))}
}
func MarshalDigest(m proto.Message) ([]byte, *repb.Digest, error) {
	b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return b, Digest(b), nil
}
func Compress(b []byte) ([]byte, error) {
	e, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	return e.EncodeAll(b, nil), nil
}
func Decompress(b []byte) ([]byte, error) {
	d, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.DecodeAll(b, nil)
}
func ResourceName(instance string, digest *repb.Digest, compressed bool) string {
	prefix := "blobs"
	if compressed {
		prefix = "compressed-blobs/zstd"
	}
	return strings.TrimPrefix(fmt.Sprintf("%s/%s/%s/%d", strings.Trim(instance, "/"), prefix, digest.Hash, digest.SizeBytes), "/")
}
func UploadResourceName(instance string, digest *repb.Digest, compressed bool) string {
	instance = strings.Trim(instance, "/")
	if instance != "" {
		instance += "/"
	}
	return instance + "uploads/" + uuid.NewString() + "/" + ResourceName("", digest, compressed)
}
func (c *Clients) Upload(ctx context.Context, instance string, b []byte) (*repb.Digest, error) {
	d := Digest(b)
	res, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: instance, DigestFunction: repb.DigestFunction_SHA256, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: b}}})
	if err != nil {
		return nil, err
	}
	if len(res.Responses) != 1 || res.Responses[0].GetStatus().GetCode() != 0 {
		return nil, fmt.Errorf("BatchUpdateBlobs: %v", res)
	}
	return d, nil
}
func (c *Clients) UploadProto(ctx context.Context, instance string, m proto.Message) (*repb.Digest, error) {
	b, _, err := MarshalDigest(m)
	if err != nil {
		return nil, err
	}
	return c.Upload(ctx, instance, b)
}
func (c *Clients) Read(ctx context.Context, instance string, d *repb.Digest) ([]byte, error) {
	stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: ResourceName(instance, d, false)})
	if err != nil {
		return nil, err
	}
	var b []byte
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return nil, err
		}
		b = append(b, r.Data...)
	}
}

// Write sends bounded messages and verifies the server's committed size.
func (c *Clients) Write(ctx context.Context, instance string, b []byte, compressed bool) (*repb.Digest, error) {
	d := Digest(b)
	data := b
	if compressed {
		var err error
		data, err = Compress(b)
		if err != nil {
			return nil, err
		}
	}
	s, err := c.ByteStream.Write(ctx)
	if err != nil {
		return nil, err
	}
	name := UploadResourceName(instance, d, compressed)
	for offset := 0; ; {
		end := offset + (1 << 20)
		if end > len(data) {
			end = len(data)
		}
		if err := s.Send(&bspb.WriteRequest{ResourceName: name, WriteOffset: int64(offset), Data: data[offset:end], FinishWrite: end == len(data)}); err != nil {
			return nil, err
		}
		offset = end
		if end == len(data) {
			break
		}
	}
	res, err := s.CloseAndRecv()
	if err != nil {
		return nil, err
	}
	if (!compressed && res.CommittedSize != int64(len(b))) || (compressed && res.CommittedSize != -1 && res.CommittedSize != int64(len(data))) {
		return nil, fmt.Errorf("committed size %d", res.CommittedSize)
	}
	return d, nil
}
