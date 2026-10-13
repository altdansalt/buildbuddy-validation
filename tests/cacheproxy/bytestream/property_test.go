package bytestream_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var propertySeed = flag.Int64("bytestream.seed", 20261012, "seed for replaying ByteStream differential cases")

// Upload identical content via both paths, with distinct contents between cases
// so an earlier CAS hit cannot hide a later case's chunking behavior.
func chunkedWrite(ctx context.Context, c *harness.Clients, instance string, data []byte, chunks []int) (int64, error) {
	s, err := c.ByteStream.Write(ctx)
	if err != nil {
		return 0, err
	}
	name := harness.UploadResourceName(instance, harness.Digest(data), false)
	offset := 0
	for i, n := range chunks {
		resource := ""
		if i == 0 {
			resource = name
		}
		err = s.Send(&bspb.WriteRequest{ResourceName: resource, WriteOffset: int64(offset), Data: data[offset : offset+n], FinishWrite: i == len(chunks)-1})
		offset += n
		if err != nil {
			break
		}
	}
	res, err := s.CloseAndRecv()
	if err != nil {
		return 0, err
	}
	return res.CommittedSize, nil
}

func TestSeededProxyAppEquivalence(t *testing.T) {
	proxy, app, ctx := fixture(t)
	rng := rand.New(rand.NewSource(*propertySeed))
	t.Logf("replay with --test_arg=-bytestream.seed=%d", *propertySeed)
	for trial := 0; trial < 24; trial++ {
		t.Run(fmt.Sprintf("seed_%d/case_%d", *propertySeed, trial), func(t *testing.T) {
			size := rng.Intn(128<<10) + 1
			if trial%6 == 0 {
				size += 1 << 20
			}
			data := make([]byte, size)
			_, err := rng.Read(data)
			require.NoError(t, err)
			chunks := []int{0} // Exercise an empty initial message with the resource name.
			for remaining := size; remaining > 0; {
				n := 1 + rng.Intn(64<<10)
				if n > remaining {
					n = remaining
				}
				chunks = append(chunks, n)
				remaining -= n
			}
			instance := fmt.Sprintf("property/%d/%d", *propertySeed, trial)
			// Alternate which path writes first; the first upload must transfer new CAS data.
			clients := []*harness.Clients{app, proxy}
			if trial%2 == 1 {
				clients[0], clients[1] = clients[1], clients[0]
			}
			a, ea := chunkedWrite(ctx, clients[0], instance, data, chunks)
			b, eb := chunkedWrite(ctx, clients[1], instance, data, chunks)
			require.Equal(t, status.Code(ea), status.Code(eb), "chunks=%v", chunks)
			require.NoError(t, ea)
			require.Equal(t, int64(size), a)
			require.Equal(t, a, b)

			// A separate app-only blob keeps the first proxy reads cold even when writes
			// above populated the proxy. Its random contents also avoid CAS deduplication.
			cold := make([]byte, size)
			_, err = rng.Read(cold)
			require.NoError(t, err)
			d, err := app.Write(ctx, instance, cold, false)
			require.NoError(t, err)
			name := harness.ResourceName(instance, d, false)
			ranges := [][2]int64{{0, 0}, {int64(size), 0}, {-1, 0}, {0, -1}}
			for i := 0; i < 12; i++ {
				offset := int64(rng.Intn(size + 1))
				limit := int64(rng.Intn(size + 100))
				if i%3 == 0 {
					limit = 0
				}
				ranges = append(ranges, [2]int64{offset, limit})
			}
			// Randomize order so the cold request varies by seed and case.
			rng.Shuffle(len(ranges), func(i, j int) { ranges[i], ranges[j] = ranges[j], ranges[i] })
			for pass := 0; pass < 2; pass++ {
				for _, r := range ranges {
					want, directErr := read(ctx, app, name, r[0], r[1])
					got, proxyErr := read(ctx, proxy, name, r[0], r[1])
					require.Equal(t, status.Code(directErr), status.Code(proxyErr), "pass=%d offset=%d limit=%d direct=%v proxy=%v", pass, r[0], r[1], directErr, proxyErr)
					require.True(t, bytes.Equal(want, got), "pass=%d offset=%d limit=%d direct bytes=%d proxy bytes=%d", pass, r[0], r[1], len(want), len(got))
					if r[0] >= 0 && r[1] >= 0 {
						require.NoError(t, directErr)
						end := int64(size)
						if r[1] > 0 && r[0]+r[1] < end {
							end = r[0] + r[1]
						}
						require.True(t, bytes.Equal(cold[r[0]:end], got), "content oracle offset=%d limit=%d", r[0], r[1])
					} else {
						require.Equal(t, codes.OutOfRange, status.Code(directErr))
					}
				}
				full, err := proxy.Read(ctx, instance, d)
				require.NoError(t, err)
				require.True(t, bytes.Equal(cold, full))
			}
			missing := harness.ResourceName(instance, harness.Digest(append(cold, byte(0))), false)
			direct, de := read(ctx, app, missing, 0, 0)
			through, pe := read(ctx, proxy, missing, 0, 0)
			require.Equal(t, codes.NotFound, status.Code(de))
			require.Equal(t, status.Code(de), status.Code(pe))
			require.True(t, bytes.Equal(direct, through))
			for _, c := range []*harness.Clients{app, proxy} {
				got, err := c.Read(ctx, instance, harness.Digest(data))
				require.NoError(t, err)
				require.True(t, bytes.Equal(data, got))
			}
		})
	}
}
