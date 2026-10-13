package bytestream_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestQueryWriteStatus(t *testing.T) {
	ctx, c := setup(t)
	data := []byte("query status finalized upload")
	n := harness.UploadResourceName("status", harness.Digest(data), false)
	t.Run("unknown", func(t *testing.T) {
		_, e := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: n})
		if status.Code(e) != codes.NotFound {
			t.Fatalf("unknown upload: got %v; want NotFound", e)
		}
	})
	r, e := write(ctx, c, &bspb.WriteRequest{ResourceName: n, Data: data, FinishWrite: true})
	if e != nil {
		t.Fatal(e)
	}
	if r.CommittedSize != int64(len(data)) {
		t.Fatal(r)
	}
	got, e := c.Read(ctx, "status", harness.Digest(data))
	if e != nil || string(got) != string(data) {
		t.Fatalf("control read: %q %v", got, e)
	}
	t.Run("complete", func(t *testing.T) {
		r, e := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: n})
		if e != nil {
			t.Fatalf("finalized upload status: %v", e)
		}
		if !r.Complete || r.CommittedSize != int64(len(data)) {
			t.Fatalf("status=%v", r)
		}
	})
}
func TestReadPastEnd(t *testing.T) {
	ctx, c := setup(t)
	data := []byte("offset boundary")
	d, e := c.Write(ctx, "past-end", data, false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName("past-end", d, false), ReadOffset: int64(len(data) + 1)})
	if status.Code(e) != codes.OutOfRange {
		t.Fatalf("offset past end: got %v; want OutOfRange", e)
	}
}

func TestUnfinishedWriteResponse(t *testing.T) {
	ctx, c := setup(t)
	data := []byte("unfinished write must return progress")
	d := harness.Digest(data)
	n := harness.UploadResourceName("unfinished", d, false)
	r, e := write(ctx, c, &bspb.WriteRequest{ResourceName: n, Data: data[:5]})
	if e != nil {
		t.Fatalf("closing an unfinished request channel must return WriteResponse: %v", e)
	}
	if r.CommittedSize < 0 || r.CommittedSize > 5 {
		t.Fatalf("invalid progress: %v", r)
	}
	_, e = c.Read(ctx, "unfinished", d)
	if status.Code(e) != codes.NotFound {
		t.Fatalf("unfinished resource published: %v", e)
	}
}
