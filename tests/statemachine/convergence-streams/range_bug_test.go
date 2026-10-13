package streams

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"testing"
	"time"
)

// Contract assertion deliberately fails on released v2.314.0. Excluded by bug tag.
func TestCompressedRangeContract(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	want := bytes.Repeat([]byte("compressed range contract\n"), 40000)
	d, err := f.Clients(harness.CacheProxy).Write(ctx, "range", want, true)
	if err != nil {
		t.Fatal(err)
	}
	uri := "bytestream://app:1985/" + harness.ResourceName("range", d, true)
	// Positive identity and compressed full-file controls rule out missing data/auth.
	b, err := file(ctx, f.Clients(harness.App), uri)
	if err == nil {
		b, err = harness.Decompress(b)
	}
	if err = equal("compressed full control", b, want, err); err != nil {
		t.Fatal(err)
	}
	for _, compressed := range []bool{false, true} {
		uri = "bytestream://app:1985/" + harness.ResourceName("range", d, compressed)
		response := new(wrapperspb.BytesValue)
		err = f.Clients(harness.App).Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", rangeRequest(uri, 17, 328), response)
		b = response.Value
		if compressed && err == nil {
			b, err = harness.Decompress(b)
		}
		if err = equal("gRPC GetFileRange", b, want[17:328], err); err != nil {
			t.Errorf("zstd=%t: %v", compressed, err)
		}
		b, err = rest(ctx, f, "GetFileRange", map[string]any{"uri": uri, "start": 17, "end": 328})
		if err == nil {
			var r struct{ Data string }
			err = json.Unmarshal(b, &r)
			if err == nil {
				b, err = base64.StdEncoding.DecodeString(r.Data)
			}
			if compressed && err == nil {
				b, err = harness.Decompress(b)
			}
		}
		if err = equal("REST GetFileRange", b, want[17:328], err); err != nil {
			t.Errorf("zstd=%t: %v", compressed, err)
		}
	}
}
