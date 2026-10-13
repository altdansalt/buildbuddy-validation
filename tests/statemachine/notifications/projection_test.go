package notifications

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	reflection "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Use the running released app's schema to compare the WHOLE invocation across
// transports. Independent snapshots alone could preserve an existing mismatch.
func invocationSchema(t *testing.T, ctx context.Context, c *harness.Clients) protoreflect.MessageDescriptor {
	t.Helper()
	stream, err := reflection.NewServerReflectionClient(c.Conn).ServerReflectionInfo(ctx)
	require.NoError(t, err)
	defer stream.CloseSend()
	require.NoError(t, stream.Send(&reflection.ServerReflectionRequest{MessageRequest: &reflection.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: "invocation.Invocation"}}))
	response, err := stream.Recv()
	require.NoError(t, err)
	require.Nil(t, response.GetErrorResponse())
	set := &descriptorpb.FileDescriptorSet{}
	for _, raw := range response.GetFileDescriptorResponse().GetFileDescriptorProto() {
		file := &descriptorpb.FileDescriptorProto{}
		require.NoError(t, proto.Unmarshal(raw, file))
		set.File = append(set.File, file)
	}
	files, err := protodesc.NewFiles(set)
	require.NoError(t, err)
	descriptor, err := files.FindDescriptorByName("invocation.Invocation")
	require.NoError(t, err)
	return descriptor.(protoreflect.MessageDescriptor)
}
func equalInvocationProjections(t *testing.T, schema protoreflect.MessageDescriptor, wire []byte, ui map[string]any) {
	t.Helper()
	actual := dynamicpb.NewMessage(schema)
	require.NoError(t, proto.Unmarshal(normalizeWire(t, wire), actual))
	row := normalizeUI(ui)["invocation"].([]any)[0]
	encoded, err := json.Marshal(row)
	require.NoError(t, err)
	expected := dynamicpb.NewMessage(schema)
	require.NoError(t, protojson.Unmarshal(encoded, expected))
	require.True(t, proto.Equal(expected, actual), "whole invocation projections disagree: HTTP=%s gRPC=%s", expected, actual)
}
