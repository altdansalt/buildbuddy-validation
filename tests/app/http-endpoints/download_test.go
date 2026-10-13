package http_endpoints

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	bespb "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These wire fields follow the pinned release's build_event_stream.proto and
// zip.proto; no source-built component or generated BuildBuddy dependency.
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func number(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}

// Compare field records without depending on protobuf serialization order.
func equalWireFields(a, b []byte) bool {
	parse := func(b []byte) map[protowire.Number]string {
		fields := map[protowire.Number]string{}
		for len(b) > 0 {
			n, typ, k := protowire.ConsumeTag(b)
			if k < 0 {
				return nil
			}
			v := protowire.ConsumeFieldValue(n, typ, b[k:])
			if v < 0 {
				return nil
			}
			fields[n] = string(b[k : k+v])
			b = b[k+v:]
		}
		return fields
	}
	x, y := parse(a), parse(b)
	if x == nil || y == nil || len(x) != len(y) {
		return false
	}
	for k, v := range x {
		if y[k] != v {
			return false
		}
	}
	return true
}
func TestDownloadFollowups(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	rng := rand.New(rand.NewSource(20261013))
	t.Log("random seed: 20261013")
	data := make([]byte, 8<<20)
	rng.Read(data)
	makeURL := func(b []byte, name string) string {
		t.Helper()
		d, err := c.Write(ctx, "http-round2", b, false)
		if err != nil {
			t.Fatal(err)
		}
		uri := "bytestream://" + f.APIKey + "@" + strings.TrimPrefix(f.Endpoint(harness.App).InternalGRPC, "grpc://") + "/" + harness.ResourceName("http-round2", d, false)
		return f.Web.URL + "/file/download?" + url.Values{"bytestream_url": {uri}, "filename": {name}}.Encode()
	}
	download := makeURL(data, "payload.bin")
	t.Run("head", func(t *testing.T) {
		get, _ := request(t, f.Web.Client, "GET", download, "", nil, nil)
		head, b := request(t, f.Web.Client, "HEAD", download, "", nil, nil)
		if head.StatusCode != get.StatusCode || len(b) != 0 {
			t.Fatalf("HEAD status=%d bytes=%d", head.StatusCode, len(b))
		}
		for _, h := range []string{"Content-Type", "Content-Disposition"} {
			if head.Header.Get(h) != get.Header.Get(h) {
				t.Errorf("HEAD %s=%q GET=%q", h, head.Header.Get(h), get.Header.Get(h))
			}
		}
	})
	t.Run("inline_mime", func(t *testing.T) {
		for _, tc := range []struct{ name, ct string }{{"notes.txt", "application/octet-stream"}, {"page.html", "application/octet-stream"}, {"script.js", "application/octet-stream"}, {"data.bin", "application/octet-stream"}, {"image.png", "image/png"}} {
			target := strings.Replace(makeURL([]byte("fixture body"), tc.name), "/file/download?", "/file/view?", 1)
			r, b := request(t, f.Web.Client, "GET", target, "", nil, nil)
			ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			kind, p, err := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
			if r.StatusCode != 200 || string(b) != "fixture body" || ct != tc.ct || err != nil || kind != "inline" || p["filename"] != tc.name {
				t.Errorf("%s: status=%d MIME=%q disposition=%q body=%q", tc.name, r.StatusCode, ct, r.Header.Get("Content-Disposition"), b)
			}
		}
	})
	t.Run("zip_members", func(t *testing.T) {
		var archive bytes.Buffer
		zw := zip.NewWriter(&archive)
		bodies := [][]byte{nil, []byte("hello\n"), make([]byte, 65537), []byte("stored member payload")}
		rng.Read(bodies[2])
		for i, b := range bodies {
			w, err := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("dir/member-%d.txt", i), Method: []uint16{zip.Store, zip.Deflate, zip.Deflate, zip.Store}[i]})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Write(b); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		target := makeURL(archive.Bytes(), "archive.zip")
		r, b := request(t, f.Web.Client, "GET", target, "", nil, nil)
		if r.StatusCode != 200 || !bytes.Equal(b, archive.Bytes()) {
			t.Fatal("whole ZIP mismatch")
		}
		zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
		if err != nil {
			t.Fatal(err)
		}
		for i, file := range zr.File {
			off, err := file.DataOffset()
			if err != nil {
				t.Fatal(err)
			}
			entry := field(1, []byte(file.Name))
			entry = append(entry, number(2, file.UncompressedSize64)...)
			entry = append(entry, number(3, file.CompressedSize64)...)
			entry = append(entry, number(4, uint64(off-30-int64(len(file.Name))-int64(len(file.Extra))))...)
			entry = append(entry, number(5, uint64(off))...)
			compression := uint64(1)
			if file.Method == zip.Deflate {
				compression = 2
			}
			entry = append(entry, number(6, compression)...)
			entry = append(entry, number(7, uint64(file.CRC32))...)
			u := target + "&z=" + url.QueryEscape(base64.StdEncoding.EncodeToString(entry))
			r, b := request(t, f.Web.Client, "GET", u, "", nil, nil)
			if r.StatusCode != 200 || !bytes.Equal(b, bodies[i]) {
				t.Errorf("member %s status=%d bytes=%d want=%d", file.Name, r.StatusCode, len(b), len(bodies[i]))
			}
		}
	})
	t.Run("cancel_mid_download", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			req, err := http.NewRequestWithContext(ctx, "GET", download, nil)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			r, err := f.Web.Client.Do(req)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			prefix := make([]byte, 1024)
			_, err = io.ReadFull(r.Body, prefix)
			cancel()
			r.Body.Close()
			if err != nil || r.StatusCode != 200 || !bytes.Equal(prefix, data[:1024]) {
				t.Fatalf("pre-cancel read: %d %v", r.StatusCode, err)
			}
		}
		r, b := request(t, f.Web.Client, "GET", download, "", nil, nil)
		if r.StatusCode != 200 || !bytes.Equal(b, data) {
			t.Fatal("download after cancellation corrupted")
		}
		r, _ = request(t, &http.Client{Timeout: 30 * time.Second}, "GET", f.Endpoint(harness.App).HTTP+"/readyz?server-type=app", "", nil, nil)
		if r.StatusCode != 200 {
			t.Fatal("app unhealthy after cancellation")
		}
	})
	t.Run("persisted_artifacts", func(t *testing.T) {
		iid := fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
		stream, err := c.BES.PublishBuildToolEventStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		started := field(1, []byte(iid))
		started = append(started, field(4, []byte("--build_metadata=ROLE=HTTP_TEST"))...)
		started = append(started, field(5, []byte("test"))...)
		events := [][]byte{append(field(1, field(3, nil)), field(5, started)...), append(field(1, field(2, nil)), field(3, field(1, []byte("persisted-http-log\n")))...), append(append(field(1, field(9, nil)), field(14, field(3, field(1, []byte("SUCCESS"))))...), number(20, 1)...)}
		for i, event := range events {
			err = stream.Send(&bespb.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bespb.OrderedBuildEvent{StreamId: &bespb.StreamId{BuildId: iid, InvocationId: iid, Component: bespb.StreamId_TOOL}, SequenceNumber: int64(i + 1), Event: &bespb.BuildEvent{EventTime: timestamppb.Now(), Event: &bespb.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: event}}}}})
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		for {
			_, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, artifact := range []string{"raw_json", "raw_proto", "buildlog"} {
			r, b := request(t, f.Web.Client, "GET", f.Web.URL+"/file/download?"+url.Values{"invocation_id": {iid}, "artifact": {artifact}, "attempt": {"1"}}.Encode(), "", nil, nil)
			if r.StatusCode != 200 || len(b) == 0 {
				t.Fatalf("%s: status=%d body=%q", artifact, r.StatusCode, b)
			}
			if artifact == "raw_proto" {
				rest := b
				// Persistence redacts Started.options_description and moves Progress
				// output into the separately downloaded chunked build log.
				persistedStarted := field(1, []byte(iid))
				persistedStarted = append(persistedStarted, field(4, []byte("<REDACTED>"))...)
				persistedStarted = append(persistedStarted, field(5, []byte("test"))...)
				expected := [][]byte{append(field(1, field(3, nil)), field(5, persistedStarted)...), append(field(1, field(2, nil)), field(3, nil)...), events[2]}
				for i, want := range expected {
					size, n := protowire.ConsumeVarint(rest)
					if n < 0 || size > uint64(len(rest)-n) {
						t.Fatalf("raw proto event %d: invalid framing", i)
					}
					got := rest[n : n+int(size)]
					if !equalWireFields(got, want) {
						t.Errorf("raw proto event %d differs: got %x want %x", i, got, want)
					}
					rest = rest[n+int(size):]
				}
				if len(rest) != 0 {
					t.Errorf("raw proto has trailing bytes: %d", len(rest))
				}
			}
			if artifact == "raw_json" {
				var events []map[string]any
				if json.Unmarshal(b, &events) != nil || len(events) != 3 {
					t.Errorf("raw JSON events=%d body=%s", len(events), b)
				}
			}
			if artifact == "buildlog" && !bytes.Contains(b, []byte("persisted-http-log")) {
				t.Errorf("log missing progress: %q", b)
			}
		}
	})
}
