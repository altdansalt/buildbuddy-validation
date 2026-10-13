package readthrough_test

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// A fault relay forwards bytes from the released app, then terminates the RPC
// after exactly one real frame. No fabricated CAS contents enter the test.
type faultRelay struct {
	bspb.UnimplementedByteStreamServer
	backend bspb.ByteStreamClient
	fail    atomic.Bool
	frames  atomic.Int64
}

func (r *faultRelay) Read(req *bspb.ReadRequest, stream bspb.ByteStream_ReadServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	ctx, cancel := context.WithCancel(metadata.NewOutgoingContext(stream.Context(), md.Copy()))
	defer cancel()
	upstream, err := r.backend.Read(ctx, req)
	if err != nil {
		return err
	}
	for {
		frame, err := upstream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(frame); err != nil {
			return err
		}
		r.frames.Add(1)
		if r.fail.Load() {
			return status.Error(codes.Unavailable, "read-through injected backend failure after first frame")
		}
	}
}

func TestBackendFailureMidStream(t *testing.T) {
	app := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	docker := fixtureDocker(t, ctx, app.Endpoint(harness.App).Container)
	gateway, err := docker("network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", app.Network)
	require.NoError(t, err, string(gateway))
	client := app.Clients(harness.App)
	relay := &faultRelay{backend: client.ByteStream}
	relay.fail.Store(true)
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	bspb.RegisterByteStreamServer(server, relay)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	port := strings.Split(listener.Addr().String(), ":")
	target := "grpc://host.docker.internal:" + port[len(port)-1]
	proxyFixture := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.CacheProxy}, AppTarget: app.Endpoint(harness.App).InternalGRPC,
		SkipAuthBootstrap: true,
		DockerArgs:        map[harness.Component][]string{harness.CacheProxy: {"--network", app.Network, "--add-host", "host.docker.internal:" + strings.TrimSpace(string(gateway))}},
		Config:            map[harness.Component]harness.Config{harness.CacheProxy: {"cache_proxy": harness.Config{"remote_cache": target, "disable_cdc": true}}},
	})
	proxyFixture.APIKey = app.APIKey
	proxy := proxyFixture.Clients(harness.CacheProxy)
	data := make([]byte, 8<<20)
	_, err = rand.New(rand.NewSource(20261013)).Read(data)
	require.NoError(t, err)
	d, err := client.Write(ctx, instance, data, false)
	require.NoError(t, err)
	for attempt := 0; attempt < 2; attempt++ {
		got, err := read(ctx, proxy, d, 0, 0, false)
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.NotEmpty(t, got)
		require.Less(t, len(got), len(data))
		require.Equal(t, data[:len(got)], got)
		cached, err := proxy.Read(local(ctx), instance, d)
		require.Equal(t, codes.NotFound, status.Code(err), "failed read must not commit a partial CAS entry")
		require.Empty(t, cached)
	}
	require.EqualValues(t, 2, relay.frames.Load())
	relay.fail.Store(false)
	got, err := proxy.Read(ctx, instance, d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	got, err = proxy.Read(local(ctx), instance, d)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestActionCacheTTLFreshnessAndWrite(t *testing.T) {
	f, ctx := fixture(t)
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	d := harness.Digest([]byte(t.Name()))
	req := &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256}
	update := func(c *harness.Clients, text string) {
		_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256, ActionResult: &repb.ActionResult{StdoutRaw: []byte(text)}})
		require.NoError(t, err)
	}
	ttl := metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-platform.cache-proxy-action-cache-ttl", "3s")
	update(app, "initial")
	r, err := proxy.AC.GetActionResult(ttl, req)
	require.NoError(t, err)
	require.Equal(t, "initial", string(r.StdoutRaw))
	update(app, "backend replacement")
	r, err = proxy.AC.GetActionResult(ttl, req)
	require.NoError(t, err)
	require.Equal(t, "initial", string(r.StdoutRaw), "explicit TTL permits cached result")
	// Reads must not extend freshness: repeatedly read across the mtime deadline.
	deadline := time.Now().Add(8 * time.Second)
	for {
		r, err = proxy.AC.GetActionResult(ttl, req)
		require.NoError(t, err)
		if string(r.StdoutRaw) == "backend replacement" {
			break
		}
		require.Equal(t, "initial", string(r.StdoutRaw))
		require.True(t, time.Now().Before(deadline), "TTL kept stale result alive")
		time.Sleep(150 * time.Millisecond)
	}
	update(proxy, "proxy replacement")
	r, err = proxy.AC.GetActionResult(ttl, req)
	require.NoError(t, err)
	require.Equal(t, "proxy replacement", string(r.StdoutRaw))
	update(app, "latest")
	for _, value := range []string{"0s", "-1s", "invalid"} {
		noTTL := metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-platform.cache-proxy-action-cache-ttl", value)
		r, err = proxy.AC.GetActionResult(noTTL, req)
		require.NoError(t, err)
		require.Equal(t, "latest", string(r.StdoutRaw))
	}
}

func TestCASEvictionAndActionResults(t *testing.T) {
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Config: map[harness.Component]harness.Config{component: {"cache": harness.Config{"max_size_bytes": int64(2 << 20), "pebble": harness.Config{"min_eviction_age": "0s", "min_bytes_auto_zstd_compression": int64(1 << 30), "presence_cache": harness.Config{"max_entries": 0}}}}}})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
			rng := rand.New(rand.NewSource(20261013))
			data := make([]byte, 512<<10)
			rng.Read(data)
			d, err := app.Write(ctx, instance, data, false)
			require.NoError(t, err)
			got, err := proxy.Read(ctx, instance, d)
			require.NoError(t, err)
			require.Equal(t, data, got)
			action := harness.Digest([]byte(t.Name()))
			result := &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out", Digest: d}}}
			update := func() {
				_, err := app.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: action, DigestFunction: repb.DigestFunction_SHA256, ActionResult: result})
				require.NoError(t, err)
			}
			req := &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action, DigestFunction: repb.DigestFunction_SHA256}
			update()
			_, err = proxy.AC.GetActionResult(ctx, req)
			require.NoError(t, err)
			victim := app
			probeCtx := ctx
			if component == harness.CacheProxy {
				victim = proxy
				probeCtx = local(ctx)
			}
			evicted := false
			for i := 0; i < 120; i++ {
				pressure := make([]byte, 512<<10)
				rng.Read(pressure)
				_, err = victim.Write(probeCtx, instance, pressure, false)
				require.NoError(t, err)
				missing, err := victim.CAS.FindMissingBlobs(probeCtx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256})
				require.NoError(t, err)
				if len(missing.MissingBlobDigests) > 0 {
					evicted = true
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			require.True(t, evicted, "pressure must actually evict the referenced CAS blob")
			// Reinstall AC metadata to distinguish missing outputs from eviction of AC itself.
			update()
			r, err := proxy.AC.GetActionResult(ctx, req)
			if component == harness.App {
				require.Equal(t, codes.NotFound, status.Code(err))
			} else {
				require.NoError(t, err)
				require.Equal(t, d.Hash, r.OutputFiles[0].Digest.Hash)
				require.Equal(t, d.SizeBytes, r.OutputFiles[0].Digest.SizeBytes)
				got, err = proxy.Read(ctx, instance, d)
				require.NoError(t, err)
				require.True(t, bytes.Equal(data, got))
			}
		})
	}
}
