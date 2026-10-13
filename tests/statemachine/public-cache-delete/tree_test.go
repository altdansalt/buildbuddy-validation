package public_cache_delete_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func byteFields(b []byte, target protowire.Number) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("invalid tag")
		}
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				panic("invalid bytes")
			}
			if n == target {
				out = append(out, v)
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			if k < 0 {
				panic("invalid field")
			}
			b = b[k:]
		}
	}
	return out
}

// This is the frontend's actual length-prefixed HTTP streaming transport.
func webWire(t *testing.T, ctx context.Context, f *harness.Fixture, method string, payload []byte) ([][]byte, codes.Code) {
	t.Helper()
	framed := append([]byte{0}, binary.BigEndian.AppendUint32(nil, uint32(len(payload)))...)
	framed = append(framed, payload...)
	r, e := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(framed))
	require.NoError(t, e)
	r.Header.Set("Content-Type", "application/proto+prefixed")
	r.Header.Set("x-buildbuddy-api-key", f.APIKey)
	resp, e := f.Web.Client.Do(r)
	require.NoError(t, e)
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	require.NoError(t, e)
	var messages [][]byte
	for len(b) > 0 {
		require.GreaterOrEqual(t, len(b), 5)
		n := int(binary.BigEndian.Uint32(b[1:5]))
		require.GreaterOrEqual(t, len(b), 5+n)
		if b[0] == 0 {
			messages = append(messages, append([]byte(nil), b[5:5+n]...))
		}
		b = b[5+n:]
	}
	// gRPC status can be headers or trailers depending on whether a message was emitted.
	code := resp.Header.Get("grpc-status")
	if code == "" {
		code = resp.Trailer.Get("grpc-status")
	}
	if code == "" {
		return messages, codes.OK
	}
	var v int
	_, e = fmt.Sscan(code, &v)
	require.NoError(t, e)
	return messages, codes.Code(v)
}

func TestDirectoryDeleteSequence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	payload := []byte("directory child bytes")
	d, e := c.Upload(ctx, "", payload)
	require.NoError(t, e)
	directory := &repb.Directory{Files: []*repb.FileNode{{Name: "child.txt", Digest: d}}}
	encoded, e := proto.Marshal(directory)
	require.NoError(t, e)
	root := harness.Digest(encoded)
	present := false
	check := func() {
		stream, e := c.CAS.GetTree(ctx, &repb.GetTreeRequest{RootDigest: root})
		require.NoError(t, e)
		var dirs []*repb.Directory
		for {
			r, err := stream.Recv()
			if err != nil {
				if present {
					require.Equal(t, io.EOF, err)
				} else {
					require.Equal(t, codes.FailedPrecondition, status.Code(err))
				}
				break
			}
			dirs = append(dirs, r.Directories...)
		}
		if present {
			require.Len(t, dirs, 1)
			require.True(t, proto.Equal(directory, dirs[0]))
		} else {
			require.Empty(t, dirs)
		}
		db, e := proto.Marshal(root)
		require.NoError(t, e)
		req := wire(field(3, db))
		ui, e := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree", grpc.ForceCodec(codec{}))
		require.NoError(t, e)
		require.NoError(t, ui.SendMsg(&req))
		require.NoError(t, ui.CloseSend())
		var uiDirs [][]byte
		for {
			var r wire
			err := ui.RecvMsg(&r)
			if err != nil {
				if present {
					require.Equal(t, io.EOF, err)
				} else {
					require.Equal(t, codes.FailedPrecondition, status.Code(err))
				}
				break
			}
			uiDirs = append(uiDirs, byteFields(r, 2)...)
		}
		httpMessages, code := webWire(t, ctx, f, "GetTree", req)
		var httpDirs [][]byte
		for _, m := range httpMessages {
			httpDirs = append(httpDirs, byteFields(m, 2)...)
		}
		if present {
			require.Equal(t, codes.OK, code)
			require.Equal(t, [][]byte{encoded}, uiDirs)
			require.Equal(t, uiDirs, httpDirs)
		} else {
			require.Equal(t, codes.FailedPrecondition, code)
			require.Empty(t, httpDirs)
		}
		sizeReq := wire(field(2, db))
		var sizes wire
		e = retryUnavailable(ctx, func() error {
			return c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", &sizeReq, &sizes, grpc.ForceCodec(codec{}))
		})
		h, code := webWire(t, ctx, f, "GetTreeDirectorySizes", sizeReq)
		if present {
			require.NoError(t, e)
			require.Equal(t, codes.OK, code)
			require.Len(t, h, 1)
			require.Equal(t, []byte(sizes), h[0])
			entries := byteFields(sizes, 2)
			require.Len(t, entries, 1)
			require.Equal(t, uint64(root.SizeBytes+d.SizeBytes), varints(entries[0])[2])
			require.Equal(t, uint64(1), varints(entries[0])[3])
		} else {
			require.Equal(t, codes.FailedPrecondition, status.Code(e))
			require.Equal(t, codes.FailedPrecondition, code)
		}
		dest := filepath.Join(t.TempDir(), "out")
		require.NoError(t, os.MkdirAll(dest, 0755))
		cmd := exec.CommandContext(ctx, "bb", "download", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey, "--output_directory="+dest, fmt.Sprintf("%s/%d", root.Hash, root.SizeBytes))
		out, e := cmd.CombinedOutput()
		if present {
			require.NoError(t, e, string(out))
			b, e := os.ReadFile(filepath.Join(dest, "child.txt"))
			require.NoError(t, e)
			require.Equal(t, payload, b)
		} else {
			require.Error(t, e)
			require.Contains(t, string(out), "NotFound")
		}
	}
	check()
	for i := 0; i < 2; i++ {
		_, e = c.Upload(ctx, "", encoded)
		require.NoError(t, e)
		present = true
		check()
		_, e = c.Write(ctx, "", encoded, false)
		require.NoError(t, e)
		check()
		req := wire(field(1, []byte(harness.ResourceName("", root, false))))
		var res wire
		require.NoError(t, c.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &req, &res, grpc.ForceCodec(codec{})))
		present = false
		check()
	}
}

func retryUnavailable(ctx context.Context, call func() error) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		e := call()
		if status.Code(e) != codes.Unavailable || time.Now().After(deadline) {
			return e
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
