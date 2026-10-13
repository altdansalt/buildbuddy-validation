package groups_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// A normal, empty test-local codesearch namespace service. DROP_NAMESPACE is
// valid and idempotent. The released app is responsible for checking the org's
// code-search policy before forwarding; this service introduces no failures.
func indexFixture(t *testing.T, messages map[string]protoreflect.MessageDescriptor) (string, func() []string) {
	t.Helper()
	lis, e := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, e)
	server := grpc.NewServer()
	var mu sync.Mutex
	calls := []string{}
	handler := func(_ any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		req := dynamicpb.NewMessage(messages["IndexRequest"])
		if e := decode(req); e != nil {
			return nil, e
		}
		namespace := req.Get(req.Descriptor().Fields().ByName("namespace")).String()
		strategy := req.Get(req.Descriptor().Fields().ByName("replacement_strategy")).Int()
		if namespace == "" || strategy != 3 {
			return nil, fmt.Errorf("fixture received unsupported index request: %v", req)
		}
		mu.Lock()
		calls = append(calls, namespace)
		mu.Unlock()
		return dynamicpb.NewMessage(messages["IndexResponse"]), nil
	}
	server.RegisterService(&grpc.ServiceDesc{ServiceName: "codesearch.service.CodesearchService", HandlerType: (*interface{})(nil), Methods: []grpc.MethodDesc{{MethodName: "Index", Handler: handler}}}, struct{}{})
	go server.Serve(lis)
	t.Cleanup(func() { server.Stop(); lis.Close() })
	return fmt.Sprintf("grpc://host.docker.internal:%d", lis.Addr().(*net.TCPAddr).Port), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }
}
