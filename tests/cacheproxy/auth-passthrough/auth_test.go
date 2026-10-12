package authpassthrough_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// AC is group-scoped; CAS may intentionally deduplicate across groups. All
// assertions include direct upstream controls rather than assuming CAS tenancy.
func TestActionCacheNamespaces(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_anonymous_usage": false}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-platform.cache-proxy-action-cache-ttl", "1h")
	group := f.Web.CreateGroup(t, "auth passthrough second group")
	keys := []string{f.APIKey, f.Web.CreateAPIKey(t, group, "second writer", []string{"CACHE_WRITE"})}
	instances := []string{"", "tenant", "tenant/nested", "tenant-nested"}
	command := &repb.Command{Arguments: []string{"true"}}
	commandDigest, err := f.Clients(harness.App).UploadProto(ctx, "", command)
	require.NoError(t, err)
	action := &repb.Action{CommandDigest: commandDigest, InputRootDigest: harness.Digest(nil)}
	_, digest, err := harness.MarshalDigest(action)
	require.NoError(t, err)
	for g, key := range keys {
		p := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key)
		for i, instance := range instances {
			_, err := p.UploadProto(ctx, instance, command)
			require.NoError(t, err)
			_, err = p.UploadProto(ctx, instance, action)
			require.NoError(t, err)
			_, err = p.UploadProto(ctx, instance, &repb.Directory{})
			require.NoError(t, err)
			_, err = p.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: digest, ActionResult: &repb.ActionResult{ExitCode: int32(10*g + i + 1)}})
			require.NoError(t, err)
		}
	}
	// Interleave credentials on one connection to expose connection-level auth
	// caching, and read each namespace repeatedly to cover warmed local entries.
	p := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, "")
	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		for g, key := range keys {
			app := harness.Dial(t, f.Endpoint(harness.App).GRPC, key)
			for i, instance := range instances {
				want := int32(10*g + i + 1)
				for _, c := range []*harness.Clients{app, p} {
					wg.Add(1)
					go func(c *harness.Clients, key, instance string, want int32) {
						defer wg.Done()
						r, err := c.AC.GetActionResult(harness.WithAPIKey(ctx, key), &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: digest})
						if err != nil {
							t.Errorf("instance %q: %v", instance, err)
							return
						}
						if r.ExitCode != want {
							t.Errorf("instance %q exit code = %d, want %d", instance, r.ExitCode, want)
						}
					}(c, key, instance, want)
				}
			}
		}
		wg.Wait()
	}
	// A warmed action-cache entry must still require credentials.
	for _, key := range []string{"", "invalid-local-key"} {
		bad := ctx
		if key != "" {
			bad = harness.WithAPIKey(ctx, key)
		}
		_, err := p.AC.GetActionResult(bad, &repb.GetActionResultRequest{InstanceName: "tenant", ActionDigest: digest})
		require.Contains(t, []codes.Code{codes.Unauthenticated, codes.PermissionDenied}, status.Code(err))
	}
	// A second key in the same group must address the same namespace.
	sameKey := f.Web.CreateAPIKey(t, f.GroupID, "same group writer", []string{"CACHE_WRITE"})
	r, err := p.AC.GetActionResult(harness.WithAPIKey(ctx, sameKey), &repb.GetActionResultRequest{InstanceName: "tenant", ActionDigest: digest})
	require.NoError(t, err)
	require.Equal(t, int32(2), r.ExitCode)
}

func TestCASInstanceAndCredentialPassthrough(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{harness.App: {"auth": harness.Config{"enable_anonymous_usage": false}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	proxy, app := f.Clients(harness.CacheProxy), f.Clients(harness.App)
	for _, instance := range []string{"", "org/project", "org/project/blobs/nested"} {
		t.Run(fmt.Sprintf("instance=%s", instance), func(t *testing.T) {
			for _, streaming := range []bool{false, true} {
				data := []byte(fmt.Sprintf("instance %q streaming %v", instance, streaming))
				var d *repb.Digest
				var err error
				if streaming {
					d, err = proxy.Write(ctx, instance, data, false)
				} else {
					d, err = proxy.Upload(ctx, instance, data)
				}
				require.NoError(t, err)
				for _, c := range []*harness.Clients{app, proxy} {
					got, err := c.Read(ctx, instance, d)
					require.NoError(t, err)
					require.Equal(t, data, got)
					got, err = c.Read(ctx, instance+"/other", d)
					require.NoError(t, err)
					require.Equal(t, data, got, "default CAS partition deduplicates instances")
				}
				raw := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, "")
				for _, key := range []string{"", "invalid-local-key"} {
					bad := ctx
					if key != "" {
						bad = harness.WithAPIKey(ctx, key)
					}
					_, err := raw.Read(bad, instance, d)
					require.Contains(t, []codes.Code{codes.Unauthenticated, codes.PermissionDenied}, status.Code(err), "warmed streaming read key=%q", key)
					_, err = raw.CAS.FindMissingBlobs(bad, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
					require.Contains(t, []codes.Code{codes.Unauthenticated, codes.PermissionDenied}, status.Code(err), "warmed unary request key=%q", key)
				}
				// gRPC metadata field names are case-insensitive; the transport lowercases
				// this spelling, and the proxy must authenticate both unary and streaming.
				header := metadata.NewOutgoingContext(ctx, metadata.Pairs("X-BuildBuddy-Api-Key", f.APIKey))
				got, err := raw.Read(header, instance, d)
				require.NoError(t, err)
				require.Equal(t, data, got)
				missing, err := raw.CAS.FindMissingBlobs(header, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
				require.NoError(t, err)
				require.Empty(t, missing.MissingBlobDigests)
			}
		})
	}
}
