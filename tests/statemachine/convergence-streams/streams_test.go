package streams

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The file API's uri/data messages have the same wire fields as these wrappers.
// Unknown fields supply GetFileRange's int64 start=2 and end=3 without adding
// a dependency on unreleased BuildBuddy source or generated API bindings.
func rangeRequest(uri string, start, end int64) *wrapperspb.StringValue {
	r := wrapperspb.String(uri)
	b := protowire.AppendTag(nil, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(start))
	b = protowire.AppendTag(b, 3, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(end))
	r.ProtoReflect().SetUnknown(b)
	return r
}
func read(ctx context.Context, c *harness.Clients, name string, offset, limit int64) ([]byte, error) {
	s, e := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: name, ReadOffset: offset, ReadLimit: limit})
	if e != nil {
		return nil, e
	}
	var b []byte
	for {
		r, e := s.Recv()
		if e == io.EOF {
			return b, nil
		}
		if e != nil {
			return nil, e
		}
		b = append(b, r.Data...)
	}
}
func file(ctx context.Context, c *harness.Clients, uri string) ([]byte, error) {
	s, e := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
	if e != nil {
		return nil, e
	}
	if e = s.SendMsg(wrapperspb.String(uri)); e != nil {
		return nil, e
	}
	if e = s.CloseSend(); e != nil {
		return nil, e
	}
	var b []byte
	for {
		r := new(wrapperspb.BytesValue)
		e = s.RecvMsg(r)
		if e == io.EOF {
			return b, nil
		}
		if e != nil {
			return nil, e
		}
		b = append(b, r.Value...)
	}
}
func rest(ctx context.Context, f *harness.Fixture, method string, req any) ([]byte, error) {
	b, e := json.Marshal(req)
	if e != nil {
		return nil, e
	}
	r, e := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, e := http.DefaultClient.Do(r)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	b, e = io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("%s HTTP %d: %s", method, res.StatusCode, b)
	}
	return b, e
}
func equal(surface string, got, want []byte, e error) error {
	if e != nil {
		return fmt.Errorf("%s: %w", surface, e)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s: bytes differ (got %d want %d)", surface, len(got), len(want))
	}
	return nil
}
func check(ctx context.Context, f *harness.Fixture, clients []*harness.Clients, instance string, d *repb.Digest, want []byte) error {
	for i, c := range clients {
		for _, compressed := range []bool{false, true} {
			name := harness.ResourceName(instance, d, compressed)
			for _, r := range [][2]int64{{0, 0}, {17, 311}, {int64(len(want) - 101), 101}} {
				b, e := read(ctx, c, name, r[0], r[1])
				if e == nil && compressed {
					b, e = harness.Decompress(b)
				}
				expected := want[r[0]:]
				if r[1] > 0 {
					expected = expected[:r[1]]
				}
				if e = equal(fmt.Sprintf("endpoint %d BS zstd=%t range=%v", i, compressed, r), b, expected, e); e != nil {
					return e
				}
			}
		}
		for _, compressors := range [][]repb.Compressor_Value{nil, {repb.Compressor_ZSTD}} {
			res, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, AcceptableCompressors: compressors, DigestFunction: repb.DigestFunction_SHA256})
			if e != nil {
				return e
			}
			if len(res.Responses) != 1 {
				return fmt.Errorf("batch response count")
			}
			r := res.Responses[0]
			if r.GetStatus().GetCode() != 0 {
				return fmt.Errorf("batch status: %v", r.Status)
			}
			if r.Digest.Hash != d.Hash || r.Digest.SizeBytes != d.SizeBytes {
				return fmt.Errorf("batch digest mismatch")
			}
			b := r.Data
			if r.Compressor == repb.Compressor_ZSTD {
				b, e = harness.Decompress(b)
			}
			if e = equal("BatchRead", b, want, e); e != nil {
				return e
			}
		}
		missing, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
		if e != nil {
			return e
		}
		if len(missing.MissingBlobDigests) != 0 {
			return fmt.Errorf("endpoint %d missing blob", i)
		}
	}
	app := clients[0]
	for _, compressed := range []bool{false, true} {
		uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, compressed)
		b, e := file(ctx, app, uri)
		if e == nil && compressed {
			b, e = harness.Decompress(b)
		}
		if e = equal("gRPC GetFile", b, want, e); e != nil {
			return e
		}
		b, e = rest(ctx, f, "GetFile", map[string]any{"uri": uri})
		if e == nil && compressed {
			b, e = harness.Decompress(b)
		}
		if e = equal("REST GetFile", b, want, e); e != nil {
			return e
		}
	}
	uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, false)
	for _, r := range [][2]int64{{0, 137}, {17, 328}, {int64(len(want) - 101), int64(len(want))}} {
		response := new(wrapperspb.BytesValue)
		e := app.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", rangeRequest(uri, r[0], r[1]), response)
		if e = equal("gRPC range", response.Value, want[r[0]:r[1]], e); e != nil {
			return e
		}
		b, e := rest(ctx, f, "GetFileRange", map[string]any{"uri": uri, "start": r[0], "end": r[1]})
		if e != nil {
			return e
		}
		var result struct{ Data string }
		if e = json.Unmarshal(b, &result); e != nil {
			return e
		}
		b, e = base64.StdEncoding.DecodeString(result.Data)
		if e = equal("REST range", b, want[r[0]:r[1]], e); e != nil {
			return e
		}
	}
	m, e := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": 2, "digestFunction": 1}})
	if e != nil {
		return e
	}
	if fmt.Sprint(m["digestSizeBytes"]) != fmt.Sprint(d.SizeBytes) {
		return fmt.Errorf("UI metadata logical size: %v", m)
	}

	binary, e := exec.LookPath("bb")
	if e != nil {
		return fmt.Errorf("required bb download surface unavailable: %w", e)
	}
	dir, e := os.MkdirTemp("", "stream-download-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		out := filepath.Join(dir, string(component))
		cmd := exec.CommandContext(ctx, binary, "download", harness.ResourceName(instance, d, false), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+out)
		logs, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("bb download via %s: %v %s", component, err, logs)
		}
		b, err := os.ReadFile(out)
		if err = equal("bb download", b, want, err); err != nil {
			return err
		}
	}
	return nil
}
func TestStreamConvergence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	// Each creation updates the reference model; every subsequent operation checks
	// all model entries, including earlier blobs, to catch representation collisions.
	model := map[string][]byte{}
	for writer, c := range clients {
		for _, compressed := range []bool{false, true} {
			instance := "streams/nested"
			b := bytes.Repeat([]byte(fmt.Sprintf("writer=%d compressed=%t; logical content\n", writer, compressed)), 40000)
			d := harness.Digest(b)
			for _, reader := range clients {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				missing, err := reader.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
				cancel()
				if err != nil || len(missing.GetMissingBlobDigests()) != 1 {
					t.Fatalf("initial absent state: %v %v", missing, err)
				}
			}
			for repeat := 0; repeat < 3; repeat++ {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				representation := compressed
				if repeat == 2 {
					representation = !compressed
				}
				_, e := c.Write(ctx, instance, b, representation)
				cancel()
				if e != nil {
					t.Fatal(e)
				}
				model[d.Hash] = b
				for hash, want := range model {
					deadline, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					ticker := time.NewTicker(100 * time.Millisecond)
					var last error
					for {
						last = check(deadline, f, clients, instance, harness.Digest(want), want)
						if last == nil {
							break
						}
						select {
						case <-deadline.Done():
							ticker.Stop()
							cancel()
							t.Fatalf("writer=%d zstd=%t repeat=%d digest=%s: %v", writer, compressed, repeat, hash, last)
						case <-ticker.C:
						}
					}
					ticker.Stop()
					cancel()
				}
			}
		}
	}
}
