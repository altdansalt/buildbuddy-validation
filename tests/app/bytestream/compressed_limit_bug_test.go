package bytestream_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestCompressedReadLimit(t *testing.T) {
	ctx, c := setup(t)
	data := []byte("compressed limit conformance")
	d, err := c.Write(ctx, "compressed-limit", data, false)
	if err != nil {
		t.Fatal(err)
	}
	resource := harness.ResourceName("compressed-limit", d, true)
	control, err := read(ctx, c, &bspb.ReadRequest{ResourceName: resource})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := harness.Decompress(control)
	if err != nil || string(plain) != string(data) {
		t.Fatalf("control read=%q error=%v", plain, err)
	}
	got, err := read(ctx, c, &bspb.ReadRequest{ResourceName: resource, ReadLimit: 1})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("compressed ReadLimit=1: got code=%s bytes=%d error=%v; want InvalidArgument", status.Code(err), len(got), err)
	}
}
