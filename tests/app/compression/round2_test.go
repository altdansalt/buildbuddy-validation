package compression

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var roundTripSeed = flag.Int64("compression.seed", 20261012, "Replay seed for randomized compression round trips")

func chunkedWrite(ctx context.Context, c *harness.Clients, name string, wire []byte, rng *rand.Rand) (*bspb.WriteResponse, error) {
	s, err := c.ByteStream.Write(ctx)
	if err != nil {
		return nil, err
	}
	for off := 0; ; {
		end := off + 1 + rng.Intn(32768)
		if end > len(wire) {
			end = len(wire)
		}
		if err := s.Send(&bspb.WriteRequest{ResourceName: name, WriteOffset: int64(off), Data: wire[off:end], FinishWrite: end == len(wire)}); err != nil {
			break
		}
		off = end
		if off == len(wire) {
			break
		}
	}
	return s.CloseAndRecv()
}

func TestSeededRoundTripProperty(t *testing.T) {
	ctx, c := fixture(t)
	t.Logf("replay with --test_arg=-compression.seed=%d", *roundTripSeed)
	rng := rand.New(rand.NewSource(*roundTripSeed))
	for trial := 0; trial < 24; trial++ {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			sizes := []int{0, 1, 127, 4096, 65537, 262145}
			size := sizes[trial%len(sizes)]
			if trial >= len(sizes) {
				size = rng.Intn(300000)
			}
			data := make([]byte, size)
			_, err := rng.Read(data)
			require.NoError(t, err)
			if trial%3 == 0 {
				for i := range data {
					data[i] %= 4
				}
			}
			for _, compressedWrite := range []bool{false, true} {
				// Distinct payloads prevent CAS deduplication from bypassing either writer.
				payload := append(bytes.Clone(data), byte(trial), byte(0))
				if compressedWrite {
					payload[len(payload)-1] = 1
				}
				d := harness.Digest(payload)
				wire := payload
				if compressedWrite {
					wire, err = harness.Compress(payload)
					require.NoError(t, err)
				}
				instance := fmt.Sprintf("property/%d/%d", *roundTripSeed, trial)
				rsp, err := chunkedWrite(ctx, c, harness.UploadResourceName(instance, d, compressedWrite), wire, rng)
				require.NoError(t, err, "seed=%d trial=%d compressed=%v", *roundTripSeed, trial, compressedWrite)
				if compressedWrite {
					require.True(t, rsp.CommittedSize == -1 || rsp.CommittedSize == int64(len(wire)))
				} else {
					require.Equal(t, int64(len(payload)), rsp.CommittedSize)
				}
				for _, compressedRead := range []bool{false, true} {
					got, err := read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, compressedRead)})
					require.NoError(t, err)
					if compressedRead {
						got, err = harness.Decompress(got)
						require.NoError(t, err)
					}
					require.True(t, bytes.Equal(payload, got), "seed=%d trial=%d write=%v read=%v", *roundTripSeed, trial, compressedWrite, compressedRead)
				}
			}
		})
	}
}

func TestMultiframeIncompressibleUpload(t *testing.T) {
	ctx, c := fixture(t)
	rng := rand.New(rand.NewSource(31402))
	data := make([]byte, 512<<10)
	_, err := rng.Read(data)
	require.NoError(t, err)
	var wire []byte
	for _, part := range [][]byte{data[:1], data[1:65537], data[65537:]} {
		frame, err := harness.Compress(part)
		require.NoError(t, err)
		wire = append(wire, frame...)
	}
	require.Greater(t, len(wire), len(data), "incompressible input should expand on the wire")
	decoded, err := harness.Decompress(wire)
	require.NoError(t, err)
	require.Equal(t, data, decoded, "validate the multi-frame input independently")
	d := harness.Digest(data)
	_, err = chunkedWrite(ctx, c, harness.UploadResourceName("multiframe", d, true), wire, rng)
	require.NoError(t, err)
	for _, compressed := range []bool{false, true} {
		got, err := read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName("multiframe", d, compressed)})
		require.NoError(t, err)
		if compressed {
			got, err = harness.Decompress(got)
			require.NoError(t, err)
		}
		require.Equal(t, data, got)
	}
}

func TestCompressedUploadStatusAndRetry(t *testing.T) {
	ctx, c := fixture(t)
	data := bytes.Repeat([]byte("interrupted compressed upload/"), 4096)
	d := harness.Digest(data)
	wire, err := harness.Compress(data)
	require.NoError(t, err)
	name := harness.UploadResourceName("resume", d, true)
	checkStatus := func(completed bool) {
		t.Helper()
		rsp, err := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: name})
		// The released app explicitly does not implement resumable-write status.
		// Exercise the client fallback: restart from zero, not a wire-byte offset.
		if status.Code(err) == codes.Unimplemented {
			t.Logf("QueryWriteStatus unsupported; retry from zero (completed=%v)", completed)
			return
		}
		if !completed && status.Code(err) == codes.NotFound {
			return
		}
		require.NoError(t, err)
		require.Equal(t, completed, rsp.Complete)
		if !completed {
			require.GreaterOrEqual(t, rsp.CommittedSize, int64(0))
			require.LessOrEqual(t, rsp.CommittedSize, int64(len(data)))
		}
	}
	checkStatus(false)
	stream, err := c.ByteStream.Write(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&bspb.WriteRequest{ResourceName: name, Data: wire[:len(wire)/2]}))
	_, err = stream.CloseAndRecv()
	require.Error(t, err, "unfinished frame must not commit")
	checkStatus(false)
	// Restarting at zero is valid even if the server discarded the partial write.
	_, err = write(ctx, c, name, wire)
	require.NoError(t, err)
	checkStatus(true)
	got, err := c.Read(ctx, "resume", d)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestUnsupportedResourceCompressors(t *testing.T) {
	ctx, c := fixture(t)
	for _, compressor := range []string{"brotli", "deflate", "snappy", "identity", "ZSTD", "unknown"} {
		t.Run(compressor, func(t *testing.T) {
			data := []byte("unsupported resource compressor/" + compressor)
			d := harness.Digest(data)
			wire, err := harness.Compress(data)
			require.NoError(t, err)
			upload := strings.Replace(harness.UploadResourceName("unsupported", d, true), "/zstd/", "/"+compressor+"/", 1)
			_, err = write(ctx, c, upload, wire)
			require.Error(t, err)
			require.Contains(t, []codes.Code{codes.InvalidArgument, codes.Unimplemented}, status.Code(err))
			_, err = c.Read(ctx, "unsupported", d)
			require.Equal(t, codes.NotFound, status.Code(err), "invalid upload must not persist")
			_, err = c.Upload(ctx, "unsupported", data)
			require.NoError(t, err)
			download := strings.Replace(harness.ResourceName("unsupported", d, true), "/zstd/", "/"+compressor+"/", 1)
			_, err = read(ctx, c, &bspb.ReadRequest{ResourceName: download})
			require.Error(t, err)
			require.Contains(t, []codes.Code{codes.InvalidArgument, codes.Unimplemented}, status.Code(err))
		})
	}
}
