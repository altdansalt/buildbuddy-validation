package http_endpoints

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

// Reserve a loopback port for the monitoring listener, which the harness does
// not expose. Docker will fail startup rather than reuse another service if a
// process wins the small close-to-publish race.
func fixture(t *testing.T) (*harness.Fixture, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, DockerArgs: map[harness.Component][]string{harness.App: {"-p", fmt.Sprintf("127.0.0.1:%d:9090", port)}}})
	return f, fmt.Sprintf("http://127.0.0.1:%d", port)
}

func TestHTTP(t *testing.T) {
	var f *harness.Fixture
	// Registered first so harness cleanup has saved container logs by this check.
	t.Cleanup(func() {
		if f == nil {
			return
		}
		paths, err := filepath.Glob(filepath.Join(f.OutputDir, "*.logs.txt"))
		if err != nil {
			t.Error(err)
			return
		}
		for _, path := range paths {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
				continue
			}
			for _, marker := range []string{"http: panic serving", "panic:", "fatal error:"} {
				if bytes.Contains(b, []byte(marker)) {
					t.Errorf("container log %s contains %q", path, marker)
				}
			}
		}
	})
	var monitor string
	f, monitor = fixture(t)
	client := &http.Client{Timeout: 30 * time.Second}
	base := f.Endpoint(harness.App).HTTP
	t.Run("health_readiness", func(t *testing.T) {
		for _, tc := range []struct {
			path    string
			code    int
			headers map[string]string
		}{
			{"/healthz?server-type=app", 200, nil}, {"/readyz?server-type=app", 200, nil},
			{"/readyz", 200, map[string]string{"server-type": "app"}},
			{"/readyz?server-type=executor", 500, nil}, {"/readyz", 500, nil},
		} {
			r, _ := request(t, client, "GET", base+tc.path, "", nil, tc.headers)
			if r.StatusCode != tc.code {
				t.Errorf("%s: got %d want %d", tc.path, r.StatusCode, tc.code)
			}
		}
	})
	t.Run("metrics", func(t *testing.T) {
		r, b := request(t, client, "GET", monitor+"/metrics", "", nil, nil)
		ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || ct != "text/plain" || r.StatusCode != 200 {
			t.Fatalf("invalid metrics response: %d %q", r.StatusCode, r.Header.Get("Content-Type"))
		}
		for _, s := range []string{"# HELP ", "# TYPE ", "go_goroutines"} {
			if !bytes.Contains(b, []byte(s)) {
				t.Errorf("metrics missing %q", s)
			}
		}
	})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	data := bytes.Repeat([]byte("0123456789abcdef"), (8<<20)/16)
	d, err := c.Write(ctx, "http-surface", data, false)
	if err != nil {
		t.Fatal(err)
	}
	// The authority is the app's Docker-local gRPC listener; no request can
	// reach a shared service. Cache reads are verified against the uploaded hash.
	bs := f.APIKey + "@" + strings.TrimPrefix(f.Endpoint(harness.App).InternalGRPC, "grpc://") + "/" + harness.ResourceName("http-surface", d, false)
	download := f.Web.URL + "/file/download?" + url.Values{"bytestream_url": {"bytestream://" + bs}, "filename": {"dir/payload.bin"}}.Encode()
	t.Run("large_download", func(t *testing.T) {
		r, b := request(t, f.Web.Client, "GET", download, "", nil, nil)
		if r.StatusCode != 200 || !bytes.Equal(b, data) {
			t.Fatalf("download status=%d bytes=%d; want exact %d-byte blob", r.StatusCode, len(b), len(data))
		}
		if r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("content-type=%q", r.Header.Get("Content-Type"))
		}
		kind, params, err := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
		if err != nil || kind != "attachment" || params["filename"] != "payload.bin" {
			t.Errorf("disposition=%q", r.Header.Get("Content-Disposition"))
		}
	})
	t.Run("range", func(t *testing.T) {
		for _, rng := range []string{"bytes=3-17", "bytes=-16", "bytes=8388608-", "bytes=bad"} {
			r, b := request(t, f.Web.Client, "GET", download, "", nil, map[string]string{"Range": rng})
			// RFC 9110 section 14.2 allows servers to ignore Range, including invalid
			// or unsatisfiable ranges. A 200 response must contain the entire object.
			switch r.StatusCode {
			case 200:
				if !bytes.Equal(b, data) {
					t.Errorf("%s: incomplete 200 body", rng)
				}
			case 206:
				var start, end, total int
				_, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total)
				if err != nil || start < 0 || end < start || end >= len(data) || total != len(data) {
					t.Fatalf("bad Content-Range %q", r.Header.Get("Content-Range"))
				}
				if (rng == "bytes=3-17" && (start != 3 || end != 17)) || (rng == "bytes=-16" && (start != len(data)-16 || end != len(data)-1)) {
					t.Errorf("%s: unexpected selected range %d-%d", rng, start, end)
				}
				if !bytes.Equal(b, data[start:end+1]) {
					t.Errorf("%s: partial body mismatch", rng)
				}
			case 416:
				if rng != "bytes=8388608-" && rng != "bytes=bad" {
					t.Errorf("valid range rejected: %s", rng)
				}
			default:
				t.Errorf("range %s: unexpected status %d", rng, r.StatusCode)
			}
		}
	})
	t.Run("concurrent_downloads", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, b := request(t, f.Web.Client, "GET", download, "", nil, nil)
				if r.StatusCode != 200 || !bytes.Equal(b, data) {
					t.Errorf("concurrent download corrupted: %d/%d", r.StatusCode, len(b))
				}
			}()
		}
		wg.Wait()
	})
	t.Run("bad_parameters", func(t *testing.T) {
		missing := harness.Digest([]byte("not uploaded"))
		missingURL := "bytestream://" + f.APIKey + "@" + strings.TrimPrefix(f.Endpoint(harness.App).InternalGRPC, "grpc://") + "/" + harness.ResourceName("http-surface", missing, false)
		for _, tc := range []struct {
			q    url.Values
			code int
		}{
			{url.Values{}, 400}, {url.Values{"bytestream_url": {"garbage"}}, 400},
			{url.Values{"bytestream_url": {"bytestream://%zz"}}, 400},
			{url.Values{"bytestream_url": {"bytestream://" + bs}, "z": {"%%%"}}, 400},
			{url.Values{"bytestream_url": {missingURL}}, 400}, // CAS miss falls back to artifacts, requiring invocation_id.

		} {
			r, _ := request(t, f.Web.Client, "GET", f.Web.URL+"/file/download?"+tc.q.Encode(), "", nil, nil)
			if r.StatusCode != tc.code {
				t.Errorf("parameters %v: got %d want %d", tc.q, r.StatusCode, tc.code)
			}
		}
	})
	t.Run("http_rpc", func(t *testing.T) {
		endpoint := f.Web.URL + "/rpc/BuildBuddyService/GetUser"
		r, b := request(t, f.Web.Client, "POST", endpoint, "application/json", []byte("{}"), nil)
		var obj map[string]any
		if r.StatusCode != 200 || json.Unmarshal(b, &obj) != nil || obj["displayUser"] == nil {
			t.Fatalf("GetUser: %d %s", r.StatusCode, b)
		}
		for _, tc := range []struct{ ct, body string }{{"application/json", "{"}, {"application/protobuf", string([]byte{255})}, {"application/proto+prefixed", "x"}, {"text/plain", "{}"}} {
			r, _ := request(t, f.Web.Client, "POST", endpoint, tc.ct, []byte(tc.body), nil)
			if r.StatusCode != 400 {
				t.Errorf("malformed %s: status=%d", tc.ct, r.StatusCode)
			}
		}
		r, _ = request(t, f.Web.Client, "POST", f.Web.URL+"/rpc/BuildBuddyService/DoesNotExist", "application/json", []byte("{}"), nil)
		if r.StatusCode != 404 {
			t.Errorf("unknown RPC status=%d", r.StatusCode)
		}
		// Exercise the UI's gRPC framed HTTP bridge with an empty GetUser request.
		r, b = request(t, f.Web.Client, "POST", endpoint, "application/proto+prefixed", make([]byte, 5), nil)
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/proto+prefixed") || len(b) < 5 {
			t.Fatalf("framed HTTP RPC: %d %q %x", r.StatusCode, r.Header.Get("Content-Type"), b)
		}
		// gRPC-web style trailer frame carries the final status in the body.
		if !bytes.Contains(bytes.ToLower(b), []byte("grpc-status: 0")) {
			t.Errorf("missing success trailer: %q", b)
		}
	})
}
