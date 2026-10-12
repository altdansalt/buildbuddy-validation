package authgroups_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGroupAndKeyBoundaries(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a := f.Clients(harness.App)
	group := f.Web.CreateGroup(t, "isolated AC group")
	b := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, group, "writer", []string{"CACHE_WRITE"}))
	ro := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "read only", []string{}))
	cas := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "CAS only", []string{"CAS_WRITE"}))
	instance := "auth-groups"
	d := harness.Digest([]byte("same action digest in two groups"))
	put := func(c *harness.Clients, code int32) {
		_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, ActionResult: &repb.ActionResult{ExitCode: code}})
		require.NoError(t, err)
	}
	get := func(c *harness.Clients, code int32) {
		r, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d})
		require.NoError(t, err)
		require.Equal(t, code, r.ExitCode)
	}
	put(a, 11)
	_, err := b.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d})
	require.Equal(t, codes.NotFound, status.Code(err))
	put(b, 22)
	get(a, 11)
	get(b, 22)
	get(ro, 11)
	get(cas, 11)
	// v2.314.0 deliberately acknowledges unauthorized writes without storing them.
	put(ro, 33)
	put(cas, 44)
	get(a, 11)
	get(b, 22)
	for name, c := range map[string]*harness.Clients{"writer": a, "reader": ro, "cas": cas} {
		caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{InstanceName: instance})
		require.NoError(t, err)
		require.Equal(t, name == "writer", caps.GetCacheCapabilities().GetActionCacheUpdateCapabilities().GetUpdateEnabled())
	}
	data := []byte("CAS is shared across default Pebble groups")
	blob, err := cas.Upload(ctx, instance, data)
	require.NoError(t, err)
	for _, c := range []*harness.Clients{a, b, ro} {
		got, err := c.Read(ctx, instance, blob)
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
	for _, streaming := range []bool{false, true} {
		denied := []byte("read-only must not store unique bytes")
		if streaming {
			denied = append(denied, '!')
		}
		if streaming {
			_, err = ro.Write(ctx, instance, denied, false)
		} else {
			_, err = ro.Upload(ctx, instance, denied)
		}
		require.NoError(t, err)
		missing, err := a.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{harness.Digest(denied)}})
		require.NoError(t, err)
		require.Len(t, missing.MissingBlobDigests, 1)
	}
	bad := harness.Dial(t, f.Endpoint(harness.App).GRPC, "invalid-local-key")
	_, err = bad.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{blob}})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
