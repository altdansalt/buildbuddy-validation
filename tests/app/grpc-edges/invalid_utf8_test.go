package grpcedges

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type invalidUTF8Codec struct{}

func (invalidUTF8Codec) Name() string                  { return "proto" }
func (invalidUTF8Codec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (invalidUTF8Codec) Unmarshal(b []byte, v any) error {
	return proto.Unmarshal(b, v.(proto.Message))
}

func TestRejectInvalidUTF8(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Known proto3 string field 1 (instance_name), length 1, invalid UTF-8 byte.
	wire := []byte{0x0a, 0x01, 0xff}
	if err := proto.Unmarshal(wire, &repb.GetCapabilitiesRequest{}); err == nil {
		t.Fatal("control decoder unexpectedly accepted invalid UTF-8")
	}
	// A valid string with identical framing verifies the target and codec setup.
	var valid repb.ServerCapabilities
	if err := c.Conn.Invoke(ctx, "/build.bazel.remote.execution.v2.Capabilities/GetCapabilities", []byte{0x0a, 0x01, 'a'}, &valid, grpc.ForceCodec(invalidUTF8Codec{})); err != nil || valid.GetCacheCapabilities() == nil {
		t.Fatalf("valid control: %v, %v", &valid, err)
	}
	var response repb.ServerCapabilities
	err := c.Conn.Invoke(ctx, "/build.bazel.remote.execution.v2.Capabilities/GetCapabilities", wire, &response, grpc.ForceCodec(invalidUTF8Codec{}))
	if status.Code(err) != codes.Internal {
		t.Fatalf("invalid UTF-8 instance_name: got status %s (%v), response cache_capabilities=%t; want INTERNAL request-proto parsing failure", status.Code(err), err, response.GetCacheCapabilities() != nil)
	}
}
