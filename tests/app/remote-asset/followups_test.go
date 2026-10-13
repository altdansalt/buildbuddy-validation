package remoteasset_test

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"testing"
	"time"

	asset "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDigestConversion(t *testing.T) {
	const seed int64 = 731902
	t.Logf("payload seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	payload := make([]byte, 4097)
	_, err := rng.Read(payload)
	require.NoError(t, err)
	var mu sync.Mutex
	hits := 0
	c, fetch, origin, ctx := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write(payload)
	}))
	// First populate SHA256. Subsequent requests must convert the cached SRI
	// content to the requested CAS digest without contacting the missing URI.
	for i, fn := range []repb.DigestFunction_Value{repb.DigestFunction_SHA256, repb.DigestFunction_SHA1, repb.DigestFunction_SHA512, repb.DigestFunction_SHA256} {
		uri := origin + "/payload"
		if i > 0 {
			uri = origin + "/cached-only"
		}
		mu.Lock()
		before := hits
		mu.Unlock()
		resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{uri}, Qualifiers: []*asset.Qualifier{sri(string(payload))}, DigestFunction: fn})
		require.NoError(t, err)
		require.EqualValues(t, codes.OK, resp.GetStatus().GetCode(), "%v", resp)
		require.Equal(t, fn, resp.DigestFunction)
		var hash string
		switch fn {
		case repb.DigestFunction_SHA256:
			h := sha256.Sum256(payload)
			hash = hex.EncodeToString(h[:])
		case repb.DigestFunction_SHA1:
			h := sha1.Sum(payload)
			hash = hex.EncodeToString(h[:])
		case repb.DigestFunction_SHA512:
			h := sha512.Sum512(payload)
			hash = hex.EncodeToString(h[:])
		}
		require.Equal(t, hash, resp.BlobDigest.Hash)
		require.EqualValues(t, len(payload), resp.BlobDigest.SizeBytes)
		name := fmt.Sprintf("blobs/%s/%d", hash, len(payload))
		if fn != repb.DigestFunction_SHA256 {
			name = fmt.Sprintf("blobs/%s/%s/%d", map[repb.DigestFunction_Value]string{repb.DigestFunction_SHA1: "sha1", repb.DigestFunction_SHA512: "sha512"}[fn], hash, len(payload))
		}
		stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: name})
		require.NoError(t, err)
		var got []byte
		for {
			part, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			got = append(got, part.Data...)
		}
		require.Equal(t, payload, got)
		if i > 0 {
			mu.Lock()
			after := hits
			mu.Unlock()
			require.Equal(t, before, after, "conversion should use cached checksum")
		}
	}
}

func TestRedirectAndPerRequestHeaders(t *testing.T) {
	type observation struct{ path, header string }
	var mu sync.Mutex
	var seen []observation
	c, fetch, origin, ctx := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, observation{r.URL.Path, r.Header.Get("X-Fixture")})
		mu.Unlock()
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/redirect":
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
		default:
			w.Write([]byte(r.Header.Get("X-Fixture")))
		}
	}))
	for _, value := range []string{"alpha", "beta"} {
		resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{origin + "/missing", origin + "/redirect"}, Qualifiers: []*asset.Qualifier{
			{Name: "http_header:X-Fixture", Value: "shared"},
			{Name: "http_header_url:0:X-Fixture", Value: "first"},
			{Name: "http_header_url:1:X-Fixture", Value: value}, sri(value),
		}})
		require.NoError(t, err)
		require.EqualValues(t, codes.OK, resp.GetStatus().GetCode(), "%v", resp)
		data, err := c.Read(ctx, "", resp.BlobDigest)
		require.NoError(t, err)
		require.Equal(t, value, string(data))
	}
	mu.Lock()
	defer mu.Unlock()
	// setup's preflight bypasses this handler.
	require.Equal(t, []observation{{"/missing", "first"}, {"/redirect", "alpha"}, {"/final", "alpha"}, {"/missing", "first"}, {"/redirect", "beta"}, {"/final", "beta"}}, seen)
}

func TestCancellationLeavesIdenticalFetchesAlive(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	c, fetch, origin, ctx := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
			w.Write([]byte("shared cancellation payload"))
		case <-r.Context().Done():
		}
	}))
	req := func() *asset.FetchBlobRequest {
		return &asset.FetchBlobRequest{InstanceName: "cancel", Uris: []string{origin + "/slow"}, Qualifiers: []*asset.Qualifier{sri("shared cancellation payload")}}
	}
	cancelledCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cancelled := make(chan error, 1)
	go func() { _, err := fetch.FetchBlob(cancelledCtx, req()); cancelled <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	type result struct {
		response *asset.FetchBlobResponse
		err      error
	}
	results := make(chan result, 3)
	var launched sync.WaitGroup
	launched.Add(3)
	for i := 0; i < 3; i++ {
		go func() { launched.Done(); resp, err := fetch.FetchBlob(ctx, req()); results <- result{resp, err} }()
	}
	launched.Wait()
	// Hold the origin open so all RPCs overlap the cancelled caller. Coalescing
	// is optional; this checks survivor correctness without requiring hit counts.
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-cancelled:
		require.Equal(t, codes.Canceled, status.Code(err))
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(release)
	for i := 0; i < 3; i++ {
		select {
		case r := <-results:
			require.NoError(t, r.err)
			require.EqualValues(t, codes.OK, r.response.GetStatus().GetCode(), "%v", r.response)
			data, err := c.Read(ctx, "cancel", r.response.BlobDigest)
			require.NoError(t, err)
			require.Equal(t, "shared cancellation payload", string(data))
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
