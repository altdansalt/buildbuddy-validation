package remoteasset_test

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	asset "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	"github.com/stretchr/testify/require"
	details "google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestFetchRegression(t *testing.T) {
	var hits atomic.Int32
	c, fetch, origin, ctx := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/header" && r.Header.Get("X-Test") != "fixture" {
			w.WriteHeader(403)
			return
		}
		w.Write([]byte("asset content"))
	}))
	t.Run("mirror fallback and CAS readback", func(t *testing.T) {
		resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{origin + "/missing", origin + "/blob"}, Qualifiers: []*asset.Qualifier{sri("asset content")}})
		require.NoError(t, err)
		require.EqualValues(t, codes.OK, resp.GetStatus().GetCode())
		require.Equal(t, origin+"/blob", resp.Uri)
		require.Equal(t, harnessDigest("asset content"), resp.BlobDigest.GetHash())
		data, err := c.Read(ctx, "", resp.BlobDigest)
		require.NoError(t, err)
		require.Equal(t, "asset content", string(data))
		before := hits.Load()
		resp, err = fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{origin + "/missing"}, Qualifiers: []*asset.Qualifier{sri("asset content")}})
		require.NoError(t, err)
		require.EqualValues(t, codes.OK, resp.GetStatus().GetCode())
		require.Equal(t, before, hits.Load())
	})
	t.Run("unsupported qualifier details", func(t *testing.T) {
		_, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{origin + "/blob"}, Qualifiers: []*asset.Qualifier{{Name: "unknown.test", Value: "x"}}})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		found := false
		for _, d := range status.Convert(err).Details() {
			if b, ok := d.(*details.BadRequest); ok {
				for _, v := range b.FieldViolations {
					if v.Field == "qualifiers.name" {
						found = true
					}
				}
			}
		}
		require.True(t, found)
	})
	t.Run("malformed checksums", func(t *testing.T) {
		for _, value := range []string{"", "sha256-%%%", "sha256-YQ==", "unknown-YQ=="} {
			_, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{origin + "/blob"}, Qualifiers: []*asset.Qualifier{{Name: "checksum.sri", Value: value}}})
			require.Equal(t, codes.InvalidArgument, status.Code(err), value)
		}
	})
	t.Run("SRI alternatives and options", func(t *testing.T) {
		q := sri("wrong")
		q.Value += " " + sri("asset content").Value + "?option"
		resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{InstanceName: "alternatives", Uris: []string{origin + "/blob"}, Qualifiers: []*asset.Qualifier{q}})
		require.NoError(t, err)
		require.EqualValues(t, codes.OK, resp.GetStatus().GetCode())
	})
	t.Run("HTTP header qualifier", func(t *testing.T) {
		resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{origin + "/header"}, Qualifiers: []*asset.Qualifier{{Name: "http_header:X-Test", Value: "fixture"}}})
		require.NoError(t, err)
		require.EqualValues(t, codes.OK, resp.GetStatus().GetCode())
	})
	t.Run("missing origin", func(t *testing.T) {
		resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{origin + "/missing"}})
		require.NoError(t, err)
		require.EqualValues(t, codes.NotFound, resp.GetStatus().GetCode())
	})
	t.Run("unreachable local origin", func(t *testing.T) {
		l, err := net.Listen("tcp4", "0.0.0.0:0")
		require.NoError(t, err)
		u, err := url.Parse(origin)
		require.NoError(t, err)
		port := l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())
		resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{"http://" + net.JoinHostPort(u.Hostname(), strconv.Itoa(port)) + "/blob"}})
		require.NoError(t, err)
		require.EqualValues(t, codes.NotFound, resp.GetStatus().GetCode())
	})
	t.Run("concurrent equivalent fetches", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := fetch.FetchBlob(ctx, &asset.FetchBlobRequest{InstanceName: "concurrent", Uris: []string{origin + "/blob"}, Qualifiers: []*asset.Qualifier{sri("asset content")}})
				if err != nil {
					t.Errorf("FetchBlob: %v", err)
					return
				}
				if resp.GetStatus().GetCode() != 0 {
					t.Errorf("inline status: %v", resp.Status)
					return
				}
				data, err := c.Read(ctx, "concurrent", resp.BlobDigest)
				if err != nil || string(data) != "asset content" {
					t.Errorf("CAS read: %q, %v", data, err)
				}
			}()
		}
		wg.Wait()
	})

	t.Run("unsupported directory and optional push", func(t *testing.T) {
		_, err := fetch.FetchDirectory(ctx, &asset.FetchDirectoryRequest{Uris: []string{origin + "/blob"}})
		require.Equal(t, codes.Unimplemented, status.Code(err))
		push := asset.NewPushClient(c.Conn)
		d, err := c.Upload(ctx, "", []byte("asset content"))
		require.NoError(t, err)
		_, err = push.PushBlob(ctx, &asset.PushBlobRequest{Uris: []string{"urn:fixture:blob"}, BlobDigest: d})
		require.Equal(t, codes.Unimplemented, status.Code(err))
		_, err = push.PushDirectory(ctx, &asset.PushDirectoryRequest{Uris: []string{"urn:fixture:directory"}, RootDirectoryDigest: d})
		require.Equal(t, codes.Unimplemented, status.Code(err))
	})
}
