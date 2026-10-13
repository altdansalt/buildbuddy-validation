package randcas_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// REAPI requires INVALID_ARGUMENT for every nonzero compressed read_limit.
// Two bytes with limit=1 is the minimal nonempty truncated-content case.
func TestCompressedReadRejectsNonzeroLimit(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, err := f.Clients(harness.App).Write(ctx, "compressed-limit", []byte("ab"), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			c := f.Clients(component)
			stream, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName("compressed-limit", d, true), ReadLimit: 1})
			var wire []byte
			if err == nil {
				for {
					r, e := stream.Recv()
					if e != nil {
						err = e
						break
					}
					wire = append(wire, r.Data...)
				}
			}
			if status.Code(err) != codes.InvalidArgument {
				var decoded []byte
				if err == io.EOF {
					decoded, _ = harness.Decompress(wire)
				}
				t.Fatalf("compressed Read(read_limit=1): status=%v, wire_bytes=%d decoded=%q; want INVALID_ARGUMENT", err, len(wire), decoded)
			}
		})
	}
}
