package convergence_test

import (
	"context"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"io"
	"testing"
	"time"
)

// Regression repro (bug-tagged): force writes to the existing derived tree cache so that
// probabilistic production caching does not make the check flaky.
func TestDeletedTreeChild(t *testing.T) {
	for attempt := 0; attempt < 2; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			flags := []string{"--enable_cache_delete_api=true", "--cache.tree_cache_write_probability=1"}
			f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: flags}})
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
			marker, err := app.Upload(ctx, "audit-tree", []byte(fmt.Sprint(attempt)))
			require.NoError(t, err)
			leaf := &repb.Directory{Files: []*repb.FileNode{{Name: "marker", Digest: marker}}}
			deleted, err := app.UploadProto(ctx, "audit-tree", leaf)
			require.NoError(t, err)
			root := deleted
			for i := 0; i < 4; i++ {
				root, err = app.UploadProto(ctx, "audit-tree", &repb.Directory{Directories: []*repb.DirectoryNode{{Name: fmt.Sprintf("level%d", i), Digest: root}}})
				require.NoError(t, err)
			}
			tree := func(c *harness.Clients) (int, bool, error) {
				stream, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: "audit-tree", RootDigest: root})
				if err != nil {
					return 0, false, err
				}
				n := 0
				containsDeleted := false
				for {
					r, err := stream.Recv()
					if err == io.EOF {
						return n, containsDeleted, nil
					}
					if err != nil {
						return n, containsDeleted, err
					}
					n += len(r.Directories)
					for _, dir := range r.Directories {
						if proto.Equal(dir, leaf) {
							containsDeleted = true
						}
					}
				}
			}
			for _, c := range []*harness.Clients{app, proxy} {
				n, _, err := tree(c)
				require.NoError(t, err)
				require.Equal(t, 5, n)
			}
			// Tree cache finalization is asynchronous and bounded to one second in source.
			time.Sleep(2 * time.Second)
			require.NoError(t, app.Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", wrapperspb.String(harness.ResourceName("audit-tree", deleted, false)), &emptypb.Empty{}))
			_, err = app.Read(ctx, "audit-tree", deleted)
			require.Equal(t, codes.NotFound, status.Code(err))
			fm, err := app.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "audit-tree", BlobDigests: []*repb.Digest{deleted}})
			require.NoError(t, err)
			require.Len(t, fm.MissingBlobDigests, 1)
			var stale []bool
			started := time.Now()
			for {
				stale = nil
				for _, c := range []*harness.Clients{app, proxy} {
					n, containsDeleted, err := tree(c)
					require.NoError(t, err)
					require.Equal(t, 5, n)
					stale = append(stale, containsDeleted)
				}
				if time.Since(started) >= 5*time.Second {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			t.Logf("deleted child still exposed after %s app=%v proxy=%v", time.Since(started), stale[0], stale[1])
			// The frontend's streaming service adapter delegates to the same CAS server.
			rd, err := proto.Marshal(root)
			require.NoError(t, err)
			s, err := app.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree")
			require.NoError(t, err)
			require.NoError(t, s.SendMsg(wire(field(field(nil, 2, []byte("audit-tree")), 3, rd))))
			require.NoError(t, s.CloseSend())
			messages := 0
			frontendStale := false
			for {
				out := &emptypb.Empty{}
				err = s.RecvMsg(out)
				if err != nil {
					break
				}
				messages++
				for _, b := range fields(t, out.ProtoReflect().GetUnknown(), 2) {
					dir := &repb.Directory{}
					require.NoError(t, proto.Unmarshal(b, dir))
					frontendStale = frontendStale || proto.Equal(dir, leaf)
				}
			}
			t.Logf("frontend GetTree after deletion: messages=%d terminal=%v", messages, err)
			httpMessages, httpCode := frontendHTTP(t, ctx, f, "GetTree", field(field(nil, 2, []byte("audit-tree")), 3, rd))
			require.Equal(t, "0", httpCode)
			httpStale := false
			for _, msg := range httpMessages {
				for _, b := range fields(t, msg, 2) {
					dir := &repb.Directory{}
					require.NoError(t, proto.Unmarshal(b, dir))
					httpStale = httpStale || proto.Equal(dir, leaf)
				}
			}
			sizeOut := &emptypb.Empty{}
			require.NoError(t, app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", wire(field(field(nil, 2, rd), 3, []byte("audit-tree"))), sizeOut))
			require.Len(t, fields(t, sizeOut.ProtoReflect().GetUnknown(), 2), 5)
			t.Logf("frontend deleted child gRPC=%v HTTP=%v; directory sizes still include all five", frontendStale, httpStale)
			stale = append(stale, frontendStale, httpStale)
			for _, containsDeleted := range stale {
				require.False(t, containsDeleted, "GetTree exposed a deleted child")
			}
		})
	}
}
