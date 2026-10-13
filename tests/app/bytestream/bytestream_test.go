package bytestream_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func setup(t *testing.T) (context.Context, *harness.Clients) {
	t.Helper()
	// Register before Start so this runs after the harness preserves container logs.
	var f *harness.Fixture
	t.Cleanup(func() {
		if f == nil {
			return
		}
		paths, _ := filepath.Glob(filepath.Join(f.OutputDir, "*app.logs.txt"))
		for _, p := range paths {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Logf("read preserved app log: %v", err)
				continue
			}
			lines := strings.Split(string(b), "\n")
			for i, line := range lines {
				if strings.Contains(strings.ToLower(line), "panic") {
					end := i + 24
					if end > len(lines) {
						end = len(lines)
					}
					t.Logf("app container panic excerpt:\n%s", strings.Join(lines[i:end], "\n"))
					if !t.Failed() {
						t.Error("unexpected panic in app container log")
					}
					break
				}
			}
		}
	})
	f = harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx, f.Clients(harness.App)
}
func read(ctx context.Context, c *harness.Clients, req *bspb.ReadRequest) ([]byte, error) {
	s, e := c.ByteStream.Read(ctx, req)
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
			return b, e
		}
		b = append(b, r.Data...)
	}
}
func write(ctx context.Context, c *harness.Clients, reqs ...*bspb.WriteRequest) (*bspb.WriteResponse, error) {
	s, e := c.ByteStream.Write(ctx)
	if e != nil {
		return nil, e
	}
	for _, r := range reqs {
		if e = s.Send(r); e != nil && e != io.EOF {
			return nil, e
		}
	}
	return s.CloseAndRecv()
}
func TestReadRanges(t *testing.T) {
	ctx, c := setup(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), 20000)
	d, e := c.Write(ctx, "ranges", data, false)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name       string
		off, limit int64
		want       []byte
		code       codes.Code
	}{
		{"all", 0, 0, data, codes.OK}, {"slice", 17, 31, data[17:48], codes.OK},
		{"tail", int64(len(data) - 7), 100, data[len(data)-7:], codes.OK},
		{"at-end", int64(len(data)), 0, nil, codes.OK},
		{"negative-offset", -1, 0, nil, codes.OutOfRange}, {"negative-limit", 0, -1, nil, codes.OutOfRange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, e := read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName("ranges", d, false), ReadOffset: tc.off, ReadLimit: tc.limit})
			if status.Code(e) != tc.code {
				t.Fatalf("code=%v error=%v want=%v", status.Code(e), e, tc.code)
			}
			if !bytes.Equal(b, tc.want) {
				t.Fatalf("read %d bytes, want %d", len(b), len(tc.want))
			}
		})
	}
	for _, name := range []string{"", "blobs/not-a-hash/1", "blobs/abcd/-1"} {
		_, e := read(ctx, c, &bspb.ReadRequest{ResourceName: name})
		if status.Code(e) != codes.InvalidArgument {
			t.Errorf("resource %q: %v", name, e)
		}
	}
	_, e = read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName("", harness.Digest([]byte("missing")), false)})
	if status.Code(e) != codes.NotFound {
		t.Fatal(e)
	}
}
func TestWriteValidationAndRecovery(t *testing.T) {
	ctx, c := setup(t)
	for _, tc := range []struct {
		name     string
		requests func(string) []*bspb.WriteRequest
	}{
		{"missing-name", func(n string) []*bspb.WriteRequest {
			return []*bspb.WriteRequest{{Data: []byte("abc"), FinishWrite: true}}
		}},
		{"download-name", func(n string) []*bspb.WriteRequest {
			return []*bspb.WriteRequest{{ResourceName: harness.ResourceName("", harness.Digest([]byte("abc")), false), Data: []byte("abc"), FinishWrite: true}}
		}},
		{"initial-offset", func(n string) []*bspb.WriteRequest {
			return []*bspb.WriteRequest{{ResourceName: n, WriteOffset: 1, Data: []byte("abc"), FinishWrite: true}}
		}},
		{"gap", func(n string) []*bspb.WriteRequest {
			return []*bspb.WriteRequest{{ResourceName: n, Data: []byte("a")}, {WriteOffset: 2, Data: []byte("bc"), FinishWrite: true}}
		}},
		{"changed-name", func(n string) []*bspb.WriteRequest {
			return []*bspb.WriteRequest{{ResourceName: n, Data: []byte("a")}, {ResourceName: n + "/other", WriteOffset: 1, Data: []byte("bc"), FinishWrite: true}}
		}},
		{"short-finish", func(n string) []*bspb.WriteRequest {
			return []*bspb.WriteRequest{{ResourceName: n, Data: []byte("ab"), FinishWrite: true}}
		}},
		{"wrong-hash", func(n string) []*bspb.WriteRequest {
			return []*bspb.WriteRequest{{ResourceName: n, Data: []byte("xyz"), FinishWrite: true}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := harness.Digest([]byte("abc"))
			n := harness.UploadResourceName(tc.name, d, false)
			_, e := write(ctx, c, tc.requests(n)...)
			if status.Code(e) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument: %v", e)
			}
			_, e = c.Read(ctx, tc.name, d)
			if status.Code(e) != codes.NotFound {
				t.Fatalf("invalid write published data: %v", e)
			}
		})
	}
	t.Run("interrupted-restart", func(t *testing.T) {
		data := []byte("interrupted upload restart")
		d := harness.Digest(data)
		n := harness.UploadResourceName("restart", d, false)
		partial, cancel := context.WithCancel(ctx)
		s, e := c.ByteStream.Write(partial)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Send(&bspb.WriteRequest{ResourceName: n, Data: data[:5]}); e != nil {
			t.Fatal(e)
		}
		cancel()
		_, _ = s.CloseAndRecv()
		_, e = c.Read(ctx, "restart", d)
		if status.Code(e) != codes.NotFound {
			t.Fatalf("unfinished data visible: %v", e)
		}
		r, e := write(ctx, c, &bspb.WriteRequest{ResourceName: n, Data: data[:5]}, &bspb.WriteRequest{WriteOffset: 5}, &bspb.WriteRequest{WriteOffset: 5, Data: data[5:]}, &bspb.WriteRequest{WriteOffset: int64(len(data)), FinishWrite: true})
		if e != nil {
			t.Fatal(e)
		}
		if r.CommittedSize != int64(len(data)) {
			t.Fatal(r)
		}
		got, e := c.Read(ctx, "restart", d)
		if e != nil || !bytes.Equal(got, data) {
			t.Fatalf("readback: %q %v", got, e)
		}
	})
}
func TestConcurrentSameDigest(t *testing.T) {
	ctx, c := setup(t)
	data := bytes.Repeat([]byte("concurrent digest\n"), 65536)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, e := c.Write(ctx, "concurrent", data, false); errs <- e }()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Error(e)
		}
	}
	got, e := c.Read(ctx, "concurrent", harness.Digest(data))
	if e != nil || !bytes.Equal(got, data) {
		t.Fatalf("readback length=%d error=%v", len(got), e)
	}
}
