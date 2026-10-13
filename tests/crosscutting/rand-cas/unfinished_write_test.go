package randcas_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	bspb "google.golang.org/genproto/googleapis/bytestream"
)

// ByteStream requires a WriteResponse on request-channel closure even when
// finish_write is absent; completion remains false until finish_write is sent.
func TestUnfinishedWriteReturnsResponse(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			c := f.Clients(component)
			d := harness.Digest([]byte("ab"))
			s, err := c.ByteStream.Write(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = s.Send(&bspb.WriteRequest{ResourceName: harness.UploadResourceName("unfinished", d, false), Data: []byte("a")})
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.CloseAndRecv()
			if err != nil {
				t.Fatalf("half-close without finish_write: %v; want WriteResponse", err)
			}
			if res.CommittedSize < 0 || res.CommittedSize > 1 {
				t.Fatalf("invalid committed size: %v", res)
			}
		})
	}
}
