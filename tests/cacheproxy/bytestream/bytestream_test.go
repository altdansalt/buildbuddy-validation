package bytestream_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fixture(t *testing.T) (*harness.Clients, *harness.Clients, context.Context) {
	t.Helper()
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return f.Clients(harness.CacheProxy), f.Clients(harness.App), ctx
}

func read(ctx context.Context, c *harness.Clients, name string, offset, limit int64) ([]byte, error) {
	s, err := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: name, ReadOffset: offset, ReadLimit: limit})
	if err != nil {
		return nil, err
	}
	var data []byte
	for {
		r, err := s.Recv()
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return data, err
		}
		data = append(data, r.Data...)
	}
}

func TestReadRanges(t *testing.T) {
	proxy, app, ctx := fixture(t)
	data := make([]byte, 2<<20+137)
	rand.New(rand.NewSource(1)).Read(data)
	cases := []struct {
		name          string
		offset, limit int64
	}{
		{"full", 0, 0}, {"one-byte", 0, 1}, {"interior", 17, 12345},
		{"frame-boundary", (1 << 20) - 3, 11}, {"tail", int64(len(data)) - 7, 100},
		{"unlimited-tail", int64(len(data)) - 7, 0}, {"at-eof", int64(len(data)), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Separate instances AND contents ensure each first read is a proxy miss.
			blob := append([]byte(nil), data...)
			blob[0] = byte(tc.offset + tc.limit)
			instance := "ranges/" + tc.name
			d, err := app.Write(ctx, instance, blob, false)
			require.NoError(t, err)
			name := harness.ResourceName(instance, d, false)
			end := int64(len(blob))
			if tc.limit > 0 && tc.offset+tc.limit < end {
				end = tc.offset + tc.limit
			}
			for pass := 0; pass < 2; pass++ {
				got, err := read(ctx, proxy, name, tc.offset, tc.limit)
				require.NoError(t, err)
				require.True(t, bytes.Equal(blob[tc.offset:end], got), "pass %d: expected %d bytes, got %d", pass, end-tc.offset, len(got))
			}
			got, err := proxy.Read(ctx, instance, d)
			require.NoError(t, err)
			require.True(t, bytes.Equal(blob, got), "partial reads must not populate corrupt full blobs")
		})
	}
	for _, tc := range []struct {
		name          string
		offset, limit int64
		code          codes.Code
	}{
		{"negative-offset", -1, 0, codes.OutOfRange}, {"negative-limit", 0, -1, codes.OutOfRange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := proxy.Write(ctx, "invalid", data, false)
			require.NoError(t, err)
			_, err = read(ctx, proxy, harness.ResourceName("invalid", d, false), tc.offset, tc.limit)
			require.Equal(t, tc.code, status.Code(err))
		})
	}
	_, err := read(ctx, proxy, harness.ResourceName("missing", harness.Digest([]byte("absent")), false), 0, 0)
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestWritesAndCancellation(t *testing.T) {
	proxy, app, ctx := fixture(t)
	t.Run("offset-errors", func(t *testing.T) {
		for _, offsets := range [][]int64{{1}, {-1}, {0, 4}, {0, 0}} {
			s, err := proxy.ByteStream.Write(ctx)
			require.NoError(t, err)
			name := harness.UploadResourceName("bad-offset", harness.Digest([]byte("abcdef")), false)
			for i, offset := range offsets {
				err = s.Send(&bspb.WriteRequest{ResourceName: name, WriteOffset: offset, Data: []byte("abc"), FinishWrite: i == len(offsets)-1})
				if err != nil {
					break
				}
			}
			_, err = s.CloseAndRecv()
			require.Equal(t, codes.InvalidArgument, status.Code(err), "offsets %v: %v", offsets, err)
		}
	})
	t.Run("cancel-and-restart", func(t *testing.T) {
		data := bytes.Repeat([]byte("interrupted upload"), 100000)
		d := harness.Digest(data)
		name := harness.UploadResourceName("restart", d, false)
		writeCtx, cancel := context.WithCancel(ctx)
		s, err := proxy.ByteStream.Write(writeCtx)
		require.NoError(t, err)
		require.NoError(t, s.Send(&bspb.WriteRequest{ResourceName: name, Data: data[:1<<20]}))
		cancel()
		_, err = s.CloseAndRecv()
		require.Equal(t, codes.Canceled, status.Code(err))
		// The pinned app deliberately does not implement QueryWriteStatus. Restart
		// from zero is its documented fallback; never assume Send means committed.
		_, err = proxy.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: name})
		require.Equal(t, codes.Unimplemented, status.Code(err))
		retry, err := proxy.ByteStream.Write(ctx)
		require.NoError(t, err)
		require.NoError(t, retry.Send(&bspb.WriteRequest{ResourceName: name, Data: data[:1<<20]}))
		require.NoError(t, retry.Send(&bspb.WriteRequest{WriteOffset: 1 << 20, Data: data[1<<20:], FinishWrite: true}))
		res, err := retry.CloseAndRecv()
		require.NoError(t, err)
		require.Equal(t, int64(len(data)), res.CommittedSize)
		for _, c := range []*harness.Clients{proxy, app} {
			got, err := c.Read(ctx, "restart", d)
			require.NoError(t, err)
			require.True(t, bytes.Equal(data, got))
		}
	})
	t.Run("concurrent-large", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				data := make([]byte, 16<<20+53)
				rand.New(rand.NewSource(int64(i + 50))).Read(data)
				d, err := proxy.Write(ctx, "large", data, false)
				require.NoError(t, err)
				for _, c := range []*harness.Clients{app, proxy} {
					got, err := c.Read(ctx, "large", d)
					require.NoError(t, err)
					require.True(t, bytes.Equal(data, got))
				}
				readCtx, cancel := context.WithCancel(ctx)
				s, err := proxy.ByteStream.Read(readCtx, &bspb.ReadRequest{ResourceName: harness.ResourceName("large", d, false)})
				require.NoError(t, err)
				_, err = s.Recv()
				require.NoError(t, err)
				cancel()
				for {
					_, err = s.Recv()
					if err != nil {
						break
					}
				}
				require.Equal(t, codes.Canceled, status.Code(err))
				got, err := proxy.Read(ctx, "large", d)
				require.NoError(t, err)
				require.True(t, bytes.Equal(data, got))
			})
		}
	})
}
