package smoke_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

func TestProxyRoundTrip(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	proxy, app := f.Clients(harness.CacheProxy), f.Clients(harness.App)
	caps, err := proxy.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, caps.CacheCapabilities)
	data := []byte("released cache proxy forwards this blob to the local app\n")
	d, err := proxy.Write(ctx, "proxy-smoke", data, false)
	require.NoError(t, err)
	// A direct app read proves the proxy forwarded the write upstream.
	got, err := app.Read(ctx, "proxy-smoke", d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	got, err = proxy.Read(ctx, "proxy-smoke", d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	// Also exercise proxy CAS batch writes and reads.
	batch := []byte("batch CAS round trip\n")
	bd, err := proxy.Upload(ctx, "proxy-smoke", batch)
	require.NoError(t, err)
	read, err := proxy.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "proxy-smoke", Digests: []*repb.Digest{bd}, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	require.Len(t, read.Responses, 1)
	require.Zero(t, read.Responses[0].GetStatus().GetCode())
	require.Equal(t, batch, read.Responses[0].Data)
	got, err = app.Read(ctx, "proxy-smoke", bd)
	require.NoError(t, err)
	require.Equal(t, batch, got)

	// Prove the local self-auth user can own multiple orgs with separate keys.
	secondGroup := f.Web.CreateGroup(t, "second smoke org")
	secondKey := f.Web.CreateAPIKey(t, secondGroup, "second org key", []string{"CACHE_WRITE"})
	second := harness.Dial(t, f.Endpoint(harness.App).GRPC, secondKey)
	_, err = second.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{
		InstanceName: "proxy-smoke", BlobDigests: []*repb.Digest{bd}, DigestFunction: repb.DigestFunction_SHA256,
	})
	require.NoError(t, err)
	require.NotEqual(t, f.GroupID, secondGroup)
	require.NotEqual(t, f.APIKey, secondKey)
}
