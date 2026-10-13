package public_cache_delete_test

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
	"strings"
	"testing"
	"time"
)

func TestCrossClientWrites(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	key2 := f.Web.CreateAPIKey(t, f.GroupID, "second-writer", []string{"CACHE_WRITE"})
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy), harness.Dial(t, f.Endpoint(harness.App).GRPC, key2), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key2)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	instance := "conformance/nested"
	model := map[string][]byte{}
	var action *repb.Digest
	var result *repb.ActionResult
	acPresent := false
	consistent := func() error {
		for i, c := range clients {
			for _, b := range model {
				d := harness.Digest(b)
				missing, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
				if e != nil {
					return e
				}
				if len(missing.MissingBlobDigests) != 0 {
					return fmt.Errorf("client %d missing %s", i, d.Hash)
				}
				batch, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
				if e != nil {
					return e
				}
				if len(batch.Responses) != 1 || !proto.Equal(batch.Responses[0].Digest, d) || batch.Responses[0].Status.Code != 0 || string(batch.Responses[0].Data) != string(b) {
					return fmt.Errorf("client %d batch mismatch", i)
				}
				got, e := c.Read(ctx, instance, d)
				if e != nil {
					return e
				}
				if string(got) != string(b) {
					return fmt.Errorf("client %d byte mismatch", i)
				}
			}
			if result != nil {
				got, e := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: action})
				if !acPresent {
					if status.Code(e) != codes.NotFound {
						return fmt.Errorf("client %d deleted AC status %s", i, status.Code(e))
					}
					continue
				}
				if e != nil {
					return e
				}
				if !proto.Equal(got, result) {
					return fmt.Errorf("client %d AC mismatch", i)
				}
			}
		}
		return nil
	}
	check := func() {
		start := time.Now()
		deadline := start.Add(15 * time.Second)
		for {
			e := consistent()
			if e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("convergence deadline: %v", e)
			}
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		t.Logf("convergence=%s", time.Since(start))
		for i := 0; i < 3; i++ {
			require.NoError(t, consistent())
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	upload := func(i int, b []byte) *repb.Digest {
		d, e := clients[i].Upload(ctx, instance, b)
		require.NoError(t, e)
		model[d.Hash] = b
		check()
		return d
	}
	output := upload(1, []byte("same organization cross-client output"))
	rootBytes, e := proto.Marshal(&repb.Directory{Files: []*repb.FileNode{{Name: "out.txt", Digest: output}}})
	require.NoError(t, e)
	root := upload(2, rootBytes)
	commandBytes, e := proto.Marshal(&repb.Command{Arguments: []string{"sh", "-c", "printf output"}, OutputPaths: []string{"out.txt"}})
	require.NoError(t, e)
	command := upload(3, commandBytes)
	actionBytes, e := proto.Marshal(&repb.Action{CommandDigest: command, InputRootDigest: root})
	require.NoError(t, e)
	action = upload(0, actionBytes)
	for i, exit := range []int32{0, 7, 8, 8, 0} {
		acPresent = true
		result = &repb.ActionResult{ExitCode: exit, ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: "fixture-worker"}, OutputFiles: []*repb.OutputFile{{Path: "out.txt", Digest: output}}}
		got, e := clients[i%4].AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: action, ActionResult: result})
		require.NoError(t, e)
		require.True(t, proto.Equal(result, got))
		check()
	}

	for i := 0; i < 2; i++ {
		suffix := fmt.Sprintf("%s/%d", action.Hash, action.SizeBytes)
		resource := strings.TrimSuffix(harness.ResourceName(instance, action, false), suffix) + "ac/" + suffix
		req := wire(field(1, []byte(resource)))
		var res wire
		require.NoError(t, clients[i*2].Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &req, &res, grpc.ForceCodec(codec{})))
		acPresent = false
		check()
		require.NoError(t, clients[i*2].Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &req, &res, grpc.ForceCodec(codec{})))
		check()
		_, e := clients[1+i*2].AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: action, ActionResult: result})
		require.NoError(t, e)
		acPresent = true
		check()
	}
	for i := 0; i < 4; i++ {
		_, e := clients[i].Write(ctx, instance, model[output.Hash], i%2 == 0)
		require.NoError(t, e)
		check()
	}
}
