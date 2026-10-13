package largeblobs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const instance = "large-blobs"

func fixture(t *testing.T) *harness.Fixture {
	return harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App, harness.CacheProxy},
		Config: map[harness.Component]harness.Config{
			harness.App:        {"cache": harness.Config{"max_size_bytes": int64(8 << 30)}},
			harness.CacheProxy: {"cache": harness.Config{"max_size_bytes": int64(8 << 30)}},
		},
		// A successful 2 GiB round trip under this hard cgroup limit detects whole-blob buffering.
		DockerArgs: map[harness.Component][]string{
			harness.App:        {"--memory=1536m", "--memory-swap=1536m"},
			harness.CacheProxy: {"--memory=1536m", "--memory-swap=1536m"},
		},
	})
}

func blob(t *testing.T, size int64, seed int64) (*os.File, *repb.Digest) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "blob")
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	// Repeat deterministic random 1 MiB chunks: nontrivial data without a blob-sized allocation.
	chunk := make([]byte, 1<<20)
	_, err = rand.New(rand.NewSource(size + seed)).Read(chunk)
	require.NoError(t, err)
	h := sha256.New()
	for n := int64(0); n < size; {
		count := int64(len(chunk))
		if count > size-n {
			count = size - n
		}
		_, err = io.MultiWriter(f, h).Write(chunk[:count])
		require.NoError(t, err)
		n += count
	}
	_, err = f.Seek(0, 0)
	require.NoError(t, err)
	return f, &repb.Digest{Hash: hex.EncodeToString(h.Sum(nil)), SizeBytes: size}
}

func upload(ctx context.Context, c *harness.Clients, name string, r io.Reader, offset int64) (*bspb.WriteResponse, error) {
	s, err := c.ByteStream.Write(ctx)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1<<20)
	for {
		n, e := r.Read(buf)
		if e != nil && e != io.EOF {
			return nil, e
		}
		err = s.Send(&bspb.WriteRequest{ResourceName: name, WriteOffset: offset, Data: buf[:n], FinishWrite: e == io.EOF})
		if err != nil {
			return s.CloseAndRecv()
		}
		offset += int64(n)
		if e == io.EOF {
			return s.CloseAndRecv()
		}
	}
}

type streamReader struct {
	s       bspb.ByteStream_ReadClient
	pending []byte
}

func (r *streamReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		m, e := r.s.Recv()
		if e != nil {
			return 0, e
		}
		r.pending = m.Data
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func verify(t *testing.T, ctx context.Context, c *harness.Clients, f *os.File, d *repb.Digest, compressed bool, offset, limit int64) {
	t.Helper()
	s, e := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, compressed), ReadOffset: offset, ReadLimit: limit})
	require.NoError(t, e)
	var r io.Reader = &streamReader{s: s}
	if compressed {
		z, e := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
		require.NoError(t, e)
		defer z.Close()
		r = z
	}
	h := sha256.New()
	n, e := io.Copy(h, r)
	require.NoError(t, e)
	want := d.SizeBytes - offset
	if limit > 0 && limit < want {
		want = limit
	}
	require.Equal(t, want, n)
	expected := sha256.New()
	_, e = f.Seek(offset, 0)
	require.NoError(t, e)
	_, e = io.CopyN(expected, f, want)
	require.NoError(t, e)
	require.Equal(t, hex.EncodeToString(expected.Sum(nil)), hex.EncodeToString(h.Sum(nil)))
	if offset == 0 && limit == 0 {
		require.Equal(t, d.Hash, hex.EncodeToString(h.Sum(nil)))
	}
}

func TestLargeRoundTrips(t *testing.T) {
	f := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	for _, size := range []int64{64 << 20, 256 << 20, 2 << 30} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var disk syscall.Statfs_t
			require.NoError(t, syscall.Statfs(os.TempDir(), &disk))
			free := uint64(disk.Bavail) * uint64(disk.Bsize)
			t.Logf("free runner bytes=%d blob=%d", free, size)
			// Source, encoded upload, app/proxy storage, plus daemon/image headroom.
			if free < uint64(size)*4+(4<<30) {
				t.Skipf("insufficient disk for bounded %d-byte case", size)
			}
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				for _, compressed := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/compressed=%v", component, compressed), func(t *testing.T) {
						// Unique content forces each route and compression mode to upload the full blob.
						seed := int64(1)
						if component == harness.CacheProxy {
							seed += 2
						}
						if compressed {
							seed++
						}
						data, d := blob(t, size, seed)
						c := f.Clients(component)
						_, e := data.Seek(0, 0)
						require.NoError(t, e)
						var source io.Reader = data
						if compressed {
							encoded, e := os.CreateTemp(t.TempDir(), "zstd")
							require.NoError(t, e)
							defer encoded.Close()
							z, e := zstd.NewWriter(encoded, zstd.WithEncoderConcurrency(1))
							require.NoError(t, e)
							_, e = io.Copy(z, data)
							require.NoError(t, e)
							require.NoError(t, z.Close())
							_, e = encoded.Seek(0, 0)
							require.NoError(t, e)
							source = encoded
						}
						res, e := upload(ctx, c, harness.UploadResourceName(instance, d, compressed), source, 0)
						require.NoError(t, e)
						if !compressed {
							require.Equal(t, size, res.CommittedSize)
						}
						for _, reader := range []harness.Component{harness.App, harness.CacheProxy} {
							rc := f.Clients(reader)
							verify(t, ctx, rc, data, d, false, 0, 0)
							verify(t, ctx, rc, data, d, true, 0, 0)
							for _, offset := range []int64{size - (1 << 20) - 7, size - 17, size} {
								verify(t, ctx, rc, data, d, false, offset, 0)
								verify(t, ctx, rc, data, d, false, offset, 13)
								verify(t, ctx, rc, data, d, true, offset, 0)
							}
						}
					})
				}
			}
		})
	}
}

func TestInterruptedWriteRestart(t *testing.T) {
	f := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			data, d := blob(t, 64<<20, int64(len(component)))
			c := f.Clients(component)
			name := harness.UploadResourceName(instance, d, false)
			s, e := c.ByteStream.Write(ctx)
			require.NoError(t, e)
			chunk := make([]byte, 1<<20)
			_, e = io.ReadFull(data, chunk)
			require.NoError(t, e)
			require.NoError(t, s.Send(&bspb.WriteRequest{ResourceName: name, Data: chunk}))
			_, _ = s.CloseAndRecv()
			q, e := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: name})
			var offset int64
			switch status.Code(e) {
			case codes.Unimplemented, codes.NotFound:
				t.Logf("restart required: %v", e)
			case codes.OK:
				require.False(t, q.Complete)
				offset = q.CommittedSize
				require.GreaterOrEqual(t, offset, int64(0))
				require.LessOrEqual(t, offset, int64(len(chunk)))
			default:
				require.NoError(t, e)
			}
			_, e = data.Seek(offset, 0)
			require.NoError(t, e)
			_, e = upload(ctx, c, name, data, offset)
			require.NoError(t, e)
			verify(t, ctx, c, data, d, false, 0, 0)
		})
	}
}

func TestLargeWriteValidation(t *testing.T) {
	f := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			data, d := blob(t, 64<<20, 1234+int64(len(component)))
			bad := &repb.Digest{Hash: d.Hash, SizeBytes: d.SizeBytes + 1}
			c := f.Clients(component)
			_, err := upload(ctx, c, harness.UploadResourceName(instance, bad, false), data, 0)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "wrong declared size: %v", err)
			bad = &repb.Digest{Hash: fmt.Sprintf("%064x", 1), SizeBytes: d.SizeBytes}
			_, err = data.Seek(0, 0)
			require.NoError(t, err)
			_, err = upload(ctx, c, harness.UploadResourceName(instance, bad, false), data, 0)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "wrong digest: %v", err)
			_, err = data.Seek(0, 0)
			require.NoError(t, err)
			_, err = upload(ctx, c, harness.UploadResourceName(instance, d, false), data, 0)
			require.NoError(t, err)
			for _, req := range []*bspb.ReadRequest{
				{ResourceName: harness.ResourceName(instance, d, false), ReadOffset: -1},
				{ResourceName: harness.ResourceName(instance, d, false), ReadLimit: -1},
			} {
				s, e := c.ByteStream.Read(ctx, req)
				if e == nil {
					_, e = s.Recv()
				}
				require.Equal(t, codes.OutOfRange, status.Code(e))
			}
			verify(t, ctx, c, data, d, false, d.SizeBytes-19, 1<<20)
		})
	}
}
