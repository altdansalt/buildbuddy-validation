package streams

import (
	"context"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"io"
	"os/exec"
	"testing"
	"time"
)

func TestDirectoryStreamTransitions(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	// CAS updates create new digests. Repeat writes preserve prior tree versions.
	model := []*repb.Directory{}
	for step := 0; step < 4; step++ {
		dir := &repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: fmt.Sprintf("target-%d", step/2)}}}
		data := mustMarshal(dir)
		d, err := clients[step%2].Write(ctx, "trees", data, step%2 == 1)
		if err != nil {
			t.Fatal(err)
		}
		if step%2 == 0 {
			model = append(model, dir)
		}
		started := time.Now()
		for stablePass := 0; stablePass < 2; stablePass++ {
			if stablePass == 1 {
				time.Sleep(150 * time.Millisecond)
			}
			for _, want := range model {
				digest := harness.Digest(mustMarshal(want))
				if err := check(ctx, f, clients, "trees", digest, mustMarshal(want)); err != nil {
					t.Fatal(err)
				}
				for _, c := range clients {
					stream, err := c.CAS.GetTree(ctx, &repb.GetTreeRequest{InstanceName: "trees", RootDigest: digest})
					if err != nil {
						t.Fatal(err)
					}
					var got []*repb.Directory
					for {
						r, e := stream.Recv()
						if e == io.EOF {
							break
						}
						if e != nil {
							t.Fatal(e)
						}
						got = append(got, r.Directories...)
					}
					if len(got) != 1 || !proto.Equal(got[0], want) {
						t.Fatalf("tree mismatch: %v", got)
					}
				}
				req := map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "instanceName": "trees", "rootDigest": map[string]any{"hash": digest.Hash, "sizeBytes": fmt.Sprint(digest.SizeBytes)}, "digestFunction": 1}
				request := fieldBytes(nil, 2, []byte("trees"))
				request = fieldBytes(request, 3, mustMarshal(digest))
				request = fieldInt(request, 6, 1)
				frames, e := uiStream(ctx, f, "GetTree", request)
				if e != nil {
					t.Fatal(e)
				}
				var dirs []*repb.Directory
				for _, frame := range frames {
					for _, data := range byteFields(frame, 2) {
						d := new(repb.Directory)
						if e := proto.Unmarshal(data, d); e != nil {
							t.Fatal(e)
						}
						dirs = append(dirs, d)
					}
				}
				if len(dirs) != 1 || !proto.Equal(dirs[0], want) {
					t.Fatalf("UI streamed tree mismatch: %v", dirs)
				}
				stream, e := clients[0].Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/buildbuddy.service.BuildBuddyService/GetTree")
				if e != nil {
					t.Fatal(e)
				}
				if e = stream.SendMsg(wire(request)); e != nil {
					t.Fatal(e)
				}
				if e = stream.CloseSend(); e != nil {
					t.Fatal(e)
				}
				dirs = nil
				for {
					response := new(emptypb.Empty)
					e := stream.RecvMsg(response)
					if e == io.EOF {
						break
					}
					if e != nil {
						t.Fatal(e)
					}
					for _, data := range byteFields(response.ProtoReflect().GetUnknown(), 2) {
						d := new(repb.Directory)
						if e := proto.Unmarshal(data, d); e != nil {
							t.Fatal(e)
						}
						dirs = append(dirs, d)
					}
				}
				if len(dirs) != 1 || !proto.Equal(dirs[0], want) {
					t.Fatalf("gRPC UI tree mismatch: %v", dirs)
				}

				r, e := f.Web.RPC("GetTreeDirectorySizes", req)
				if e != nil {
					t.Fatal(e)
				}
				sizes, ok := r["sizes"].([]any)
				if !ok || len(sizes) != 1 {
					t.Fatalf("UI tree sizes: %v", r)
				}
				size := sizes[0].(map[string]any)
				if size["digest"] != digest.Hash || fmt.Sprint(size["totalSize"]) != fmt.Sprint(digest.SizeBytes) {
					t.Fatalf("UI cumulative size: %v", r)
				}
				sizeReq := fieldBytes(nil, 2, mustMarshal(digest))
				sizeReq = fieldBytes(sizeReq, 3, []byte("trees"))
				sizeReq = fieldInt(sizeReq, 4, 1)
				response := new(emptypb.Empty)
				if e := clients[0].Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetTreeDirectorySizes", wire(sizeReq), response); e != nil {
					t.Fatal(e)
				}
				checkSize := func(data []byte) {
					entries := byteFields(data, 2)
					if len(entries) != 1 {
						t.Fatalf("gRPC size count %d", len(entries))
					}
					hash := byteFields(entries[0], 1)
					fields := ints(entries[0])
					if len(hash) != 1 || string(hash[0]) != digest.Hash || fields[2] != digest.SizeBytes || fields[3] != 0 {
						t.Fatalf("gRPC sizes disagree: %v", fields)
					}
				}
				checkSize(response.ProtoReflect().GetUnknown())
				frames, e = uiStream(ctx, f, "GetTreeDirectorySizes", sizeReq)
				if e != nil {
					t.Fatal(e)
				}
				if len(frames) != 1 {
					t.Fatalf("UI framed size count %d", len(frames))
				}
				checkSize(frames[0])
				for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
					cmd := exec.CommandContext(ctx, "bb", "download", harness.ResourceName("trees", digest, false), "--type=Directory", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey)
					data, e := cmd.CombinedOutput()
					if e != nil {
						t.Fatalf("CLI Directory JSON: %v %s", e, data)
					}
					got := new(repb.Directory)
					if e := protojson.Unmarshal(data, got); e != nil {
						t.Fatal(e)
					}
					if !proto.Equal(got, want) {
						t.Fatalf("CLI Directory JSON mismatch: %v", got)
					}
				}

			}
		}
		t.Logf("tree step=%d digest=%s convergence_upper_bound=%s stable_after=150ms", step, d.Hash, time.Since(started))
	}
}
