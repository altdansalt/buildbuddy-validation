package structures_test

import (
	"bytes"
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func TestSpliceReadSurfaces(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--cache.min_chunked_read_fallback_size_bytes=1048576"}, harness.CacheProxy: {"--cache_proxy.disable_cdc=false"}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	var content []byte
	var chunks []*repb.Digest
	for i := 0; i < 2; i++ {
		b := bytes.Repeat([]byte{byte(i + 1)}, 1<<20)
		d, err := app.Upload(ctx, instance, b)
		require.NoError(t, err)
		chunks = append(chunks, d)
		content = append(content, b...)
	}
	d := harness.Digest(content)
	_, err := app.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{BlobDigest: d, ChunkDigests: chunks, DigestFunction: repb.DigestFunction_SHA256})
	require.NoError(t, err)
	for _, c := range []*harness.Clients{app, proxy} {
		got, err := c.Read(ctx, instance, d)
		require.NoError(t, err)
		require.Equal(t, content, got)
	}
	started := time.Now()
	// Retry all exposing surfaces for 10s; unlimited reads above establish that
	// the acknowledged digest exists and auth/fixture/chunk ordering are valid.
	var rangeErrors [2]error
	var fileErr, metadataErr error
	for time.Since(started) < 10*time.Second {
		for i, c := range []*harness.Clients{app, proxy} {
			got, err := streamBytes(ctx, c, "/google.bytestream.ByteStream/Read", number(number(field(nil, 1, []byte("blobs/"+key(d))), 2, 1), 3, 10))
			rangeErrors[i] = err
			if err == nil && !bytes.Equal(got, content[1:11]) {
				t.Fatalf("range bytes mismatch")
			}
		}
		uri := "bytestream://app:1985/blobs/" + key(d)
		var raw []byte
		fileErr = app.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", number(number(field(nil, 1, []byte(uri)), 2, 1), 3, 11), &raw, grpc.ForceCodec(wireCodec{}))
		dg, _ := proto.Marshal(d)
		rn := number(number(field(nil, 1, dg), 4, 2), 5, 1)
		metadataErr = app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", field(nil, 2, rn), &raw, grpc.ForceCodec(wireCodec{}))
		if rangeErrors[0] == nil && rangeErrors[1] == nil && fileErr == nil && metadataErr == nil {
			t.Logf("splice read convergence %s", time.Since(started))
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	for i, err := range rangeErrors {
		if err != nil {
			t.Errorf("after %s ByteStream limited read side=%d: %v", time.Since(started), i, err)
		}
	}
	if fileErr != nil {
		t.Errorf("GetFileRange: %v", fileErr)
	}
	if metadataErr != nil {
		t.Errorf("GetCacheMetadata: %v", metadataErr)
	}
}
