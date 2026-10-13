package storagegc_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var replaySeed = flag.Int64("storage_gc_seed", 314002, "Replay seed for randomized disk-cache operations")

func waitRestart(t *testing.T, f *harness.Fixture) {
	t.Helper()
	refreshEndpoints(t, f)
	client := &http.Client{Timeout: 2 * time.Second}
	require.Eventually(t, func() bool {
		r, err := client.Get(f.Endpoint(harness.App).HTTP + "/readyz?server-type=app")
		if err != nil {
			return false
		}
		defer r.Body.Close()
		return r.StatusCode == 200
	}, time.Minute, 200*time.Millisecond)
}

// The fallback writer names files <digest>.<10 alphanumeric characters>.tmp.
// Seed aged crash artifacts while the app is stopped, avoiding a 15-minute wait.
// This complements the real interrupted-upload test; O_TMPFILE may leave no name.
func TestStartupStaleFileCleanup(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%t", v2), func(t *testing.T) {
			f := fixture(t, v2, 16<<20)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			c := f.Clients(harness.App)
			data := bytes.Repeat([]byte("preserved"), 8192)
			d, err := c.Write(ctx, "", data, false)
			require.NoError(t, err)
			name := f.Endpoint(harness.App).Container
			docker(t, f, "kill", "--signal=KILL", name)
			// Place artifacts alongside the actual committed blob in either layout.
			script := `p=$(find /data/disk-cache -type f -name '*` + d.Hash + `*' | head -n 1); test -n "$p" || exit 1; printf partial > "$p.abcdefghij.tmp"; touch "$p.0123456789.tmp"; touch -t 200001010000 "$p.abcdefghij.tmp" "$p.0123456789.tmp"; printf recent > "$p.klmnopqrst.tmp"`
			docker(t, f, "run", "--rm", "--network=none", "--volumes-from", name, "redis:7.4.2-alpine@sha256:02419de7eddf55aa5bcf49efb74e88fa8d931b4d77c07eff8a6b2144472b6952", "sh", "-ec", script)
			docker(t, f, "start", name)
			waitRestart(t, f)
			c = f.Clients(harness.App)
			checkBlob(t, ctx, c, d, data, false)
			require.Eventually(t, func() bool {
				out := docker(t, f, "run", "--rm", "--network=none", "--volumes-from", name, "redis:7.4.2-alpine@sha256:02419de7eddf55aa5bcf49efb74e88fa8d931b4d77c07eff8a6b2144472b6952", "sh", "-ec", `find /data/disk-cache -type f \( -name '*.abcdefghij.tmp' -o -name '*.0123456789.tmp' \)`)
				return len(bytes.TrimSpace(out)) == 0
			}, 10*time.Second, 500*time.Millisecond, "startup scan must remove stale partial and zero-length files")
			out := docker(t, f, "run", "--rm", "--network=none", "--volumes-from", name, "redis:7.4.2-alpine@sha256:02419de7eddf55aa5bcf49efb74e88fa8d931b4d77c07eff8a6b2144472b6952", "sh", "-ec", `find /data/disk-cache -type f -name '*.klmnopqrst.tmp'`)
			require.NotEmpty(t, bytes.TrimSpace(out), "recent temporary files must not be removed")
		})
	}
}

func TestActionCacheEvictionAndReplacement(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%t", v2), func(t *testing.T) {
			f := fixture(t, v2, 256<<10)
			c := f.Clients(harness.App)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var keys []*repb.Digest
			var values []*repb.ActionResult
			for i := 0; i < 12; i++ {
				d := harness.Digest([]byte(fmt.Sprintf("action-%d", i)))
				r := &repb.ActionResult{ExitCode: int32(i), StdoutRaw: bytes.Repeat([]byte{byte(i + 1)}, 64<<10)}
				_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: r})
				require.NoError(t, err)
				keys = append(keys, d)
				values = append(values, r)
			}
			time.Sleep(time.Second)
			var evicted *repb.Digest
			misses := 0
			for i, d := range keys {
				r, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d, InlineStdout: true})
				if status.Code(err) == codes.NotFound {
					misses++
					evicted = d
					continue
				}
				require.NoError(t, err)
				require.Equal(t, values[i].ExitCode, r.ExitCode)
				require.Equal(t, values[i].StdoutRaw, r.StdoutRaw)
			}
			require.GreaterOrEqual(t, misses, 8, "AC records must participate in the disk-cache size limit")
			replacement := &repb.ActionResult{ExitCode: 42, StdoutRaw: []byte("replacement")}
			_, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: evicted, ActionResult: replacement})
			require.NoError(t, err)
			r, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: evicted, InlineStdout: true})
			require.NoError(t, err)
			require.Equal(t, replacement.ExitCode, r.ExitCode)
			require.Equal(t, replacement.StdoutRaw, r.StdoutRaw)
		})
	}
}

func TestSeededIntegrityUnderEviction(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%t", v2), func(t *testing.T) {
			t.Logf("seed=%d; replay with --test_arg=-storage_gc_seed=%d", *replaySeed, *replaySeed)
			rng := rand.New(rand.NewSource(*replaySeed))
			f := fixture(t, v2, 256<<10)
			c := f.Clients(harness.App)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			blobs := make([][]byte, 24)
			digests := make([]*repb.Digest, 24)
			for i := range blobs {
				blobs[i] = make([]byte, (1+rng.Intn(4))*16384)
				_, err := rng.Read(blobs[i])
				require.NoError(t, err)
				digests[i] = harness.Digest(blobs[i])
			}
			reads := 0
			for step := 0; step < 200; step++ {
				i := rng.Intn(len(blobs))
				op := rng.Intn(4)
				t.Logf("seed=%d step=%d op=%d blob=%d", *replaySeed, step, op, i)
				switch op {
				case 0:
					_, err := c.Write(ctx, "", blobs[i], false)
					require.NoError(t, err)
				case 1:
					b, err := c.Read(ctx, "", digests[i])
					if status.Code(err) == codes.NotFound {
						continue
					}
					require.NoError(t, err)
					require.Equal(t, blobs[i], b)
					reads++
				case 2:
					r, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{digests[i]}, DigestFunction: repb.DigestFunction_SHA256})
					require.NoError(t, err)
					require.Len(t, r.Responses, 1)
					require.Equal(t, digests[i].Hash, r.Responses[0].GetDigest().GetHash())
					require.Equal(t, digests[i].SizeBytes, r.Responses[0].GetDigest().GetSizeBytes())
					if r.Responses[0].GetStatus().GetCode() == int32(codes.NotFound) {
						continue
					}
					require.Zero(t, r.Responses[0].GetStatus().GetCode())
					require.Equal(t, blobs[i], r.Responses[0].Data)
					reads++
				case 3:
					for n := 0; n < 8; n++ {
						pressure := make([]byte, 64<<10)
						_, err := rng.Read(pressure)
						require.NoError(t, err)
						_, err = c.Write(ctx, "", pressure, false)
						require.NoError(t, err)
					}
					time.Sleep(150 * time.Millisecond)
				}
			}
			require.Positive(t, reads, "seed must exercise successful reads as well as misses")
		})
	}
}
