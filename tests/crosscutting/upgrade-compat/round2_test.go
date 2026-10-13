package upgradecompat_test

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

const oldProxy = "gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:111df7a8d4bb57993bc78f8ed18458f32a43a93e5028f906f4c6e537e10434c6"

func TestProxyPersistenceUpgrade(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Images: map[harness.Component]string{harness.App: harness.PinnedImages[harness.App], harness.CacheProxy: oldProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	const seed int64 = 2026101202
	t.Logf("payload seed=%d; proxy %s -> %s", seed, oldProxy, harness.PinnedImages[harness.CacheProxy])
	rng := rand.New(rand.NewSource(seed))
	payloads := make([][]byte, 12)
	digests := make([]*repb.Digest, len(payloads))
	for i := range payloads {
		payloads[i] = make([]byte, 1+rng.Intn(256*1024))
		_, err := rng.Read(payloads[i])
		require.NoError(t, err)
		digests[i], err = app.Upload(ctx, "proxy-upgrade", payloads[i])
		require.NoError(t, err)
		b, err := proxy.Read(ctx, "proxy-upgrade", digests[i])
		require.NoError(t, err)
		require.Equal(t, payloads[i], b)
	}
	// Flush Pebble gracefully, then deny cache fallback while retaining the local
	// app as authenticator. Successful reads must come from the retained volume.
	replaceComponent(t, f, harness.CacheProxy, harness.PinnedImages[harness.CacheProxy], func(config string) string {
		require.Contains(t, config, "remote_cache: "+f.Endpoint(harness.App).InternalGRPC)
		return strings.Replace(config, "remote_cache: "+f.Endpoint(harness.App).InternalGRPC, "remote_cache: grpc://127.0.0.1:1", 1)
	})
	proxy = f.Clients(harness.CacheProxy)
	for _, i := range rng.Perm(len(payloads)) {
		b, err := proxy.Read(ctx, "proxy-upgrade", digests[i])
		require.NoError(t, err, "seed=%d blob=%d", seed, i)
		require.Equal(t, payloads[i], b)
	}
	// A never-warmed remote blob must fail, establishing that fallback is blocked.
	d, err := app.Upload(ctx, "proxy-upgrade", []byte("remote-only control"))
	require.NoError(t, err)
	short, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	_, err = proxy.Read(short, "proxy-upgrade", d)
	require.Error(t, err)
}

func TestQueuedWorkUpgrade(t *testing.T) {
	schedulerRecovery(t, false, oldApp, harness.PinnedImages[harness.App])
}
