package convergence_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// A cache-disabled control tests the authoritative hierarchy after each mutation;
// the separate bug target exercises derived-cache invalidation.
func TestDirectoryStateMachine(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true", "--cache.enable_tree_caching=false"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	bb, err := exec.LookPath("bb")
	require.NoError(t, err)
	const instance = "hierarchy"
	content := []byte("file content\n")
	file, err := app.Upload(ctx, instance, content)
	require.NoError(t, err)
	leaf := &repb.Directory{Files: []*repb.FileNode{{Name: "file", Digest: file}}}
	raw, err := proto.Marshal(leaf)
	require.NoError(t, err)
	child := harness.Digest(raw)
	rootDir := &repb.Directory{Directories: []*repb.DirectoryNode{{Name: "child", Digest: child}}}
	rootRaw, err := proto.Marshal(rootDir)
	require.NoError(t, err)
	root := harness.Digest(rootRaw)
	childPresent, rootPresent := false, false
	check := func() {
		t.Helper()
		for _, c := range []*harness.Clients{app, proxy} {
			stream, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: instance, RootDigest: root, PageSize: 1})
			require.NoError(t, err)
			var dirs []*repb.Directory
			for {
				r, e := stream.Recv()
				if e == io.EOF {
					break
				}
				if e != nil {
					require.False(t, rootPresent && childPresent)
					require.Contains(t, []codes.Code{codes.NotFound, codes.FailedPrecondition}, status.Code(e))
					break
				}
				dirs = append(dirs, r.Directories...)
			}
			if rootPresent && childPresent {
				require.Len(t, dirs, 2)
				require.True(t, proto.Equal(rootDir, dirs[0]) || proto.Equal(rootDir, dirs[1]))
				require.True(t, proto.Equal(leaf, dirs[0]) || proto.Equal(leaf, dirs[1]))
			} else {
				for _, dir := range dirs {
					require.False(t, proto.Equal(leaf, dir) && !childPresent)
				}
			}
		}
		rd, _ := proto.Marshal(root)
		stream, err := app.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree")
		require.NoError(t, err)
		require.NoError(t, stream.SendMsg(wire(field(field(nil, 2, []byte(instance)), 3, rd))))
		require.NoError(t, stream.CloseSend())
		n := 0
		for {
			out := &emptypb.Empty{}
			e := stream.RecvMsg(out)
			if e == io.EOF {
				break
			}
			if e != nil {
				require.False(t, rootPresent && childPresent)
				require.Contains(t, []codes.Code{codes.NotFound, codes.FailedPrecondition}, status.Code(e))
				break
			}
			for _, b := range fields(t, out.ProtoReflect().GetUnknown(), 2) {
				dir := &repb.Directory{}
				require.NoError(t, proto.Unmarshal(b, dir))
				if !childPresent {
					require.False(t, proto.Equal(leaf, dir))
				}
				n++
			}
		}
		if rootPresent && childPresent {
			require.Equal(t, 2, n)
		}
		messages, code := frontendHTTP(t, ctx, f, "GetTree", field(field(nil, 2, []byte(instance)), 3, rd))
		httpCount := 0
		for _, msg := range messages {
			for _, b := range fields(t, msg, 2) {
				dir := &repb.Directory{}
				require.NoError(t, proto.Unmarshal(b, dir))
				require.True(t, proto.Equal(dir, rootDir) || proto.Equal(dir, leaf))
				if !childPresent {
					require.False(t, proto.Equal(dir, leaf))
				}
				httpCount++
			}
		}
		if rootPresent && childPresent {
			require.Equal(t, "0", code)
			require.Equal(t, 2, httpCount)
		} else {
			require.Contains(t, []string{"5", "9"}, code)
		}

		req := field(field(nil, 2, rd), 3, []byte(instance))
		out := &emptypb.Empty{}
		err = app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", wire(req), out)
		web, webErr := f.Web.RPC("GetTreeDirectorySizes", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "instanceName": instance, "rootDigest": map[string]any{"hash": root.Hash, "sizeBytes": fmt.Sprint(root.SizeBytes)}})
		if rootPresent && childPresent {
			require.NoError(t, err)
			require.NoError(t, webErr)
			sizes := fields(t, out.ProtoReflect().GetUnknown(), 2)
			require.Len(t, sizes, 2)
			for _, b := range sizes {
				hash := string(fields(t, b, 1)[0])
				total, count := uint64(child.SizeBytes+file.SizeBytes), uint64(1)
				if hash == root.Hash {
					total += uint64(root.SizeBytes)
					count = 2
				} else {
					require.Equal(t, child.Hash, hash)
				}
				require.Equal(t, total, integer(t, b, 2))
				require.Equal(t, count, integer(t, b, 3))
			}
			entries := web["sizes"].([]any)
			require.Len(t, entries, 2)
			for _, entry := range entries {
				m := entry.(map[string]any)
				expected := child.SizeBytes + file.SizeBytes
				count := 1
				if m["digest"] == root.Hash {
					expected += root.SizeBytes
					count = 2
				} else {
					require.Equal(t, child.Hash, m["digest"])
				}
				require.Equal(t, fmt.Sprint(expected), fmt.Sprint(m["totalSize"]))
				require.Equal(t, fmt.Sprint(count), fmt.Sprint(m["childCount"]))
			}
		} else {
			require.Error(t, err)
			require.Error(t, webErr)
		}
	}
	check()
	_, err = proxy.Upload(ctx, instance, raw)
	require.NoError(t, err)
	childPresent = true
	check()
	_, err = app.Write(ctx, instance, rootRaw, false)
	require.NoError(t, err)
	rootPresent = true
	check()
	namedOut := t.TempDir() + "/root.pb"
	log, err := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey, "--output_file="+namedOut, "/"+harness.ResourceName(instance, root, false)).CombinedOutput()
	require.NoError(t, err, string(log))
	namedBytes, err := os.ReadFile(namedOut)
	require.NoError(t, err)
	require.Equal(t, rootRaw, namedBytes)
	// Empty-instance extraction exercises the CLI Directory/GetTree path.
	_, err = app.Upload(ctx, "", content)
	require.NoError(t, err)
	_, err = app.Upload(ctx, "", raw)
	require.NoError(t, err)
	_, err = app.Upload(ctx, "", rootRaw)
	require.NoError(t, err)
	dest := t.TempDir()
	log, err = exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey, "--output_file="+t.TempDir()+"/root.json", "--output_directory="+dest, "/"+harness.ResourceName("", root, false)).CombinedOutput()
	require.NoError(t, err, string(log))
	got, err := os.ReadFile(dest + "/child/file")
	require.NoError(t, err)
	require.Equal(t, content, got)
	for i := 0; i < 2; i++ {
		require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", wrapperspb.String(harness.ResourceName(instance, child, false)), &emptypb.Empty{}))
		childPresent = false
		check()
	}
	_, err = app.Upload(ctx, instance, raw)
	require.NoError(t, err)
	childPresent = true
	check()
	_, err = proxy.Write(ctx, instance, raw, false)
	require.NoError(t, err)
	check()
}
