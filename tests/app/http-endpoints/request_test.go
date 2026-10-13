package http_endpoints

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func request(t *testing.T, client *http.Client, method, target, ct string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s %s: status=%d content-type=%q bytes=%d", method, r.URL.Path, resp.StatusCode, resp.Header.Get("Content-Type"), len(b))
	if resp.StatusCode >= 400 {
		t.Logf("error body: %s", b)
	}
	return resp, b
}
