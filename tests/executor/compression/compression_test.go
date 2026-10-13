package compression_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

var seed = flag.Int64("compression.seed", 314159, "Replay seed for executor compression properties")
var iterations = flag.Int("compression.iterations", 3, "Actions per configuration (1..20)")

func TestCompressionRoundTrips(t *testing.T) {
	require.GreaterOrEqual(t, *iterations, 1)
	require.LessOrEqual(t, *iterations, 20)
	t.Logf("compression seed=%d iterations=%d", *seed, *iterations)
	for _, enabled := range []bool{true, false} {
		for _, proxy := range []bool{false, true} {
			for _, upload := range []bool{true, false} {
				if !enabled && upload {
					continue
				}
				t.Run(fmt.Sprintf("advertise=%t/proxy=%t/upload=%t", enabled, proxy, upload), func(t *testing.T) {
					t.Logf("replay: -compression.seed=%d -compression.iterations=%d", *seed, *iterations)
					components := []harness.Component{harness.App, harness.Executor}
					target := harness.App
					bridge, address := startBridge(t)
					exec := harness.Config{"cache_target": "grpc://" + address}
					if proxy {
						components = append(components, harness.CacheProxy)
						target = harness.CacheProxy

					}
					f := harness.Start(t, harness.Options{Components: components, Config: map[harness.Component]harness.Config{
						harness.App:        {"cache": harness.Config{"zstd_transcoding_enabled": enabled}},
						harness.CacheProxy: {"cache": harness.Config{"zstd_transcoding_enabled": enabled}},
						harness.Executor:   {"executor": exec, "cache": harness.Config{"client": harness.Config{"enable_upload_compression": upload, "enable_download_compression": enabled}}},
					}})
					ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
					defer cancel()
					app, cache := f.Clients(harness.App), f.Clients(target)
					bridge.mu.Lock()
					bridge.conn = cache.Conn
					bridge.mu.Unlock()
					defer bridge.log(t)
					for _, c := range []*harness.Clients{app, cache} {
						caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
						require.NoError(t, err)
						require.Equal(t, enabled, hasZstd(caps.GetCacheCapabilities().GetSupportedCompressors()))
					}
					rng := rand.New(rand.NewSource(*seed))
					for iteration := 0; iteration < *iterations; iteration++ {
						t.Run(fmt.Sprintf("case=%d", iteration), func(t *testing.T) {
							runCase(t, ctx, app, cache, rng, iteration, enabled)
						})
					}
					if enabled {
						bridge.check(t, upload)
					}
				})
			}
		}
	}
}

func hasZstd(cs []repb.Compressor_Value) bool {
	for _, c := range cs {
		if c == repb.Compressor_ZSTD {
			return true
		}
	}
	return false
}

func runCase(t *testing.T, ctx context.Context, app, cache *harness.Clients, rng *rand.Rand, iteration int, compressed bool) {
	t.Helper()
	instance := fmt.Sprintf("compression/%d/%d", *seed, iteration)
	// Every action includes threshold boundaries and ByteStream-sized blobs.
	sizes := []int{0, 1, 99, 100, 101, 4096, (4 << 20) + rng.Intn(4096)}
	patterns := []bool{false, true}
	if !compressed {
		sizes = []int{0, 1, 99, 100}
		patterns = []bool{false}
	}
	runFiles(t, ctx, app, cache, rng, instance, iteration, compressed, sizes, patterns)
}

func runFiles(t *testing.T, ctx context.Context, app, cache *harness.Clients, rng *rand.Rand, instance string, iteration int, compressed bool, sizes []int, patterns []bool) {
	t.Helper()
	var files []*repb.FileNode
	var paths []string
	expected := make(map[string][]byte)
	script := ""
	for i, size := range sizes {
		for _, random := range patterns {
			b := make([]byte, size)
			if random {
				_, _ = rng.Read(b)
			} else {
				for j := range b {
					b[j] = byte('a' + iteration%2)
				}
			}
			name := fmt.Sprintf("in%d-%t", i, random)
			// Alternate identity and compressed input insertion when advertised.
			d, err := cache.Write(ctx, instance, b, compressed && i%2 == 0)
			require.NoError(t, err, "seed=%d input=%s size=%d", *seed, name, size)
			files = append(files, &repb.FileNode{Name: name, Digest: d})
			out := "out" + name
			paths = append(paths, out)
			// A unique suffix forces output upload rather than CAS deduplication.
			suffix := fmt.Sprintf("seed-%d-case-%d-file-%s", *seed, iteration, name)
			if size == 0 {
				script += fmt.Sprintf(": > %s; ", out)
				expected[out] = []byte{}
			} else if !random && size <= 101 {
				script += fmt.Sprintf("tr 'ab' 'ba' < %s > %s; ", name, out)
				expected[out] = bytes.Repeat([]byte{byte('b' - iteration%2)}, size)
			} else {
				script += fmt.Sprintf("cat %s > %s; printf '%s' >> %s; ", name, out, suffix, out)
				expected[out] = append(append([]byte{}, b...), []byte(suffix)...)
			}
		}
	}
	root, err := app.UploadProto(ctx, instance, &repb.Directory{Files: files})
	require.NoError(t, err)
	cmd, err := app.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/sh", "-ec", script}, OutputPaths: paths,
		Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}}})
	require.NoError(t, err)
	action, err := app.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(time.Minute), DoNotCache: true})
	require.NoError(t, err)
	stream, err := app.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	var result *repb.ExecuteResponse
	for {
		op, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "seed=%d", *seed)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError(), "seed=%d operation=%s", *seed, op.Name)
		result = &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(result))
		break
	}
	require.NotNil(t, result, "seed=%d", *seed)
	require.Zero(t, result.GetStatus().GetCode(), "seed=%d status=%v", *seed, result.GetStatus())
	require.Zero(t, result.GetResult().GetExitCode(), "seed=%d result=%v", *seed, result)
	require.Len(t, result.GetResult().GetOutputFiles(), len(expected))
	for _, out := range result.Result.OutputFiles {
		want, ok := expected[out.Path]
		require.True(t, ok, "unexpected output %s", out.Path)
		require.Equal(t, harness.Digest(want), out.Digest, "seed=%d output=%s", *seed, out.Path)
		for _, c := range []*harness.Clients{app, cache} {
			got, err := c.Read(ctx, instance, out.Digest)
			require.NoError(t, err)
			require.True(t, bytes.Equal(want, got), "seed=%d output=%s size=%d", *seed, out.Path, len(want))
		}
		delete(expected, out.Path)
	}
	require.Empty(t, expected)
}
