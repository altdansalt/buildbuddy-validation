package ocistate

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// OCI fetch creates an AC record readable via the proxy, but the native resource
// cannot be described by the proxy's public metadata service, even after reads.
func TestProxyACMetadataConformance(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{
		harness.App:        {"ocifetcher": harness.Config{"enabled": true}, "app": harness.Config{"client_identity": harness.Config{"key": "oci-repro-key", "client": "app"}}},
		harness.CacheProxy: {"app": harness.Config{"client_identity": harness.Config{"key": "oci-repro-key", "client": "cache-proxy"}}},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"exp": time.Now().Add(time.Minute).Unix(), "Origin": "internal", "Client": "executor"}).SignedString([]byte("oci-repro-key"))
	require.NoError(t, err)
	ctx = metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-client-identity", token)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	fetch(t, ctx, app, "FetchManifest", repo+"@sha256:"+manifestHash)
	key, inst := manifestKey(), instance+"_manifest_content_"
	cacheMetadata(t, ctx, app, inst, key, true)
	ar, err := action(ctx, proxy, inst, key)
	require.NoError(t, err)
	require.Len(t, ar.GetExecutionMetadata().GetAuxiliaryMetadata(), 1)
	req := field(nil, 2, string(resource(inst, key, true)))
	start := time.Now()
	var last error
	for time.Since(start) < 10*time.Second {
		var response []byte
		last = proxy.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", &req, &response, grpc.ForceCodec(wireCodec{}))
		if last == nil {
			cacheMetadata(t, ctx, proxy, inst, key, true)
			t.Logf("proxy metadata converged in %s", time.Since(start))
			return
		}
		require.Equal(t, codes.NotFound, status.Code(last))
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("proxy AC metadata remained absent for %s; app metadata and proxy GetActionResult exist", time.Since(start))
	require.NoError(t, last, "same valid AC resource must be observable via proxy metadata")
}
