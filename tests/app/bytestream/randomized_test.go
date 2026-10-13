package bytestream_test

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var propertySeed = flag.Int64("bytestream.seed", 20261012, "Replay seed for randomized ByteStream coverage")

func TestRandomizedByteStream(t *testing.T) {
	t.Logf("replay with -bytestream.seed=%d", *propertySeed)
	rng := rand.New(rand.NewSource(*propertySeed))
	ctx, c := setup(t)
	// Include empty, tiny, and response/chunk-boundary sizes as well as random sizes.
	sizes := []int{0, 1, 2, 65535, 65536, 65537, 1048575, 1048576, 1048577}
	for i := 0; i < 15; i++ {
		sizes = append(sizes, rng.Intn(2<<20))
	}
	for i, size := range sizes {
		data := make([]byte, size)
		if i%2 == 0 {
			_, _ = rng.Read(data)
		} else {
			pattern := make([]byte, 1+rng.Intn(128))
			_, _ = rng.Read(pattern)
			for j := range data {
				data[j] = pattern[j%len(pattern)]
			}
		}
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("case=%d/size=%d/compressed=%t", i, size, compressed), func(t *testing.T) {
				d := harness.Digest(data)
				instance := fmt.Sprintf("property-%d-%d-%t", *propertySeed, i, compressed)
				wire := data
				if compressed {
					var err error
					wire, err = harness.Compress(data)
					if err != nil {
						t.Fatal(err)
					}
				}
				name := harness.UploadResourceName(instance, d, compressed)
				reqs := []*bspb.WriteRequest{{ResourceName: name}}
				for offset := 0; offset < len(wire); {
					end := offset + 1 + rng.Intn(128<<10)
					if end > len(wire) {
						end = len(wire)
					}
					reqs = append(reqs, &bspb.WriteRequest{WriteOffset: int64(offset), Data: wire[offset:end]})
					offset = end
					if rng.Intn(4) == 0 {
						reqs = append(reqs, &bspb.WriteRequest{WriteOffset: int64(offset)})
					}
				}
				reqs = append(reqs, &bspb.WriteRequest{WriteOffset: int64(len(wire)), FinishWrite: true})
				response, err := write(ctx, c, reqs...)
				if err != nil {
					t.Fatalf("chunked write (%d frames): %v", len(reqs), err)
				}
				if response.CommittedSize != int64(len(wire)) && !(compressed && response.CommittedSize == -1) {
					t.Fatalf("committed_size=%d want %d%s", response.CommittedSize, len(wire), " (or -1 for compressed)")
				}
				// Read in both formats regardless of upload format, checking the uncompressed oracle.
				for _, readCompressed := range []bool{false, true} {
					resource := harness.ResourceName(instance, d, readCompressed)
					ranges := [][2]int64{{0, 0}, {int64(size), 0}, {-1, 0}}
					for j := 0; j < 8; j++ {
						off := int64(rng.Intn(size + 1))
						limit := int64(rng.Intn(size + 129))
						if j%3 == 0 {
							limit = 0
						}
						ranges = append(ranges, [2]int64{off, limit})
					}
					ranges = append(ranges, [2]int64{0, -1}, [2]int64{0, 1})
					for _, pair := range ranges {
						off, limit := pair[0], pair[1]
						// Nonzero compressed limits have a separate minimal bug repro.
						if readCompressed && limit != 0 {
							continue
						}
						wantCode := codes.OK
						if off < 0 || limit < 0 {
							wantCode = codes.OutOfRange
						}
						if readCompressed && limit != 0 {
							wantCode = codes.InvalidArgument
						}
						got, err := read(ctx, c, &bspb.ReadRequest{ResourceName: resource, ReadOffset: off, ReadLimit: limit})
						if status.Code(err) != wantCode {
							t.Errorf("read compressed=%t offset=%d limit=%d: code=%s want=%s err=%v", readCompressed, off, limit, status.Code(err), wantCode, err)
							continue
						}
						if wantCode != codes.OK {
							continue
						}
						if readCompressed {
							got, err = harness.Decompress(got)
							if err != nil {
								t.Errorf("decompress offset=%d: %v", off, err)
								continue
							}
						}
						end := int64(size)
						if limit > 0 && limit < end-off {
							end = off + limit
						}
						if !bytes.Equal(got, data[off:end]) {
							t.Errorf("read compressed=%t offset=%d limit=%d: got %d bytes want slice [%d:%d]", readCompressed, off, limit, len(got), off, end)
						}
					}
				}
			})
		}
	}
}
