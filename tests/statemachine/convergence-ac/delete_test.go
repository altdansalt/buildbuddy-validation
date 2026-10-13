package convergence_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// DeleteFile is opt-in, and is an app API (not a proxy RPC). Exercise the
// supported app deletion after warming all eight proxy variants.
func TestActionDeleteRecreate(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	key := f.Web.CreateAPIKey(t, f.GroupID, "delete-client", []string{"CACHE_WRITE"})
	clients = append(clients, harness.Dial(t, f.Endpoint(harness.App).GRPC, key), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key))
	cmd, err := uploadCheckedProto(t, ctx, f, clients, &repb.Command{Arguments: []string{"true"}})
	require.NoError(t, err)
	root, err := uploadCheckedProto(t, ctx, f, clients, &repb.Directory{Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "target"}}})
	require.NoError(t, err)
	action, err := uploadCheckedProto(t, ctx, f, clients, &repb.Action{CommandDigest: cmd, InputRootDigest: root})
	require.NoError(t, err)
	data := []byte("stable deletion output")
	d, err := clients[1].Write(ctx, "", data, false)
	require.NoError(t, err)
	checkBlobSurfaces(t, ctx, f, clients, d, data)
	m := model{blobs: map[string][]byte{d.Hash: data}, digests: []*repb.Digest{d}}
	for step := 0; step < 5; step++ {
		if step == 3 {
			req := wire(field(nil, 1, []byte(fmt.Sprintf("blobs/ac/%s/%d", action.Hash, action.SizeBytes))))
			require.NoError(t, clients[2].Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", req, &emptypb.Empty{}))
			started := time.Now()
			deadline := started.Add(15 * time.Second)
			for {
				err := checkAbsent(ctx, clients, action)
				if err == nil {
					err = checkAbsentHTTP(ctx, f, action)
				}
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("delete failed to converge after %s: %v", time.Since(started), err)
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Logf("delete AC all clients converged in %s", time.Since(started))
			for stable := 0; stable < 3; stable++ {
				time.Sleep(500 * time.Millisecond)
				require.NoError(t, checkAbsent(ctx, clients, action))
				require.NoError(t, checkAbsentHTTP(ctx, f, action))
			}
			_, err := metadata(ctx, clients[0], action, 1, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata")
			require.Equal(t, codes.NotFound, status.Code(err))
			_, err = f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": action.Hash, "sizeBytes": fmt.Sprint(action.SizeBytes)}, "cacheType": 1, "digestFunction": 1}})
			require.ErrorContains(t, err, "NotFound")
			continue
		}
		m.result = &repb.ActionResult{ExitCode: int32(step), ExecutionMetadata: &repb.ExecutedActionMetadata{Worker: fmt.Sprintf("lifecycle-%d", step)}, OutputFiles: []*repb.OutputFile{{Path: "result.txt", Digest: d}}}
		result, err := clients[step%len(clients)].AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: action, ActionResult: proto.Clone(m.result).(*repb.ActionResult), DigestFunction: repb.DigestFunction_SHA256})
		require.NoError(t, err)
		require.True(t, proto.Equal(m.result, result))
		started := time.Now()
		require.Eventually(t, func() bool { return checkReads(ctx, clients, action, m) == nil && checkHTTP(ctx, f, action, m) == nil }, 15*time.Second, 100*time.Millisecond)
		t.Logf("lifecycle step %d converged in %s", step, time.Since(started))
		checkACMetadataPair(t, ctx, f, clients[0], action)
		for stable := 0; stable < 3; stable++ {
			time.Sleep(500 * time.Millisecond)
			require.NoError(t, checkReads(ctx, clients, action, m))
			require.NoError(t, checkHTTP(ctx, f, action, m))
			checkACMetadataPair(t, ctx, f, clients[0], action)
		}
	}
}
func checkAbsent(ctx context.Context, clients []*harness.Clients, d *repb.Digest) error {
	for side, c := range clients {
		if side%2 == 0 {
			_, err := readActionAPI(ctx, c, d)
			// The pooled file client sanitizes all exhausted read attempts,
			// including NotFound, to this exact Unavailable status. The AC
			// RPCs below independently prove absence on live connections.
			expected := fmt.Sprintf("failed to read byte stream resource %q", fmt.Sprintf("actioncache://app:1985/blobs/ac/%s/%d", d.Hash, d.SizeBytes))
			if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != expected {
				return fmt.Errorf("API GetFile expected released missing-resource status: %v", err)
			}
		}
		for mask := 0; mask < 8; mask++ {
			req := &repb.GetActionResultRequest{ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256, InlineStdout: mask&1 != 0, InlineStderr: mask&2 != 0}
			if mask&4 != 0 {
				req.InlineOutputFiles = []string{"result.txt"}
			}
			_, err := c.AC.GetActionResult(ctx, req)
			if status.Code(err) != codes.NotFound {
				return fmt.Errorf("side %d mask %d expected NotFound: %v", side, mask, err)
			}
		}
	}
	return nil
}

func checkAbsentHTTP(ctx context.Context, f *harness.Fixture, d *repb.Digest) error {
	for _, mode := range []string{"view", "download", "api"} {
		uri := fmt.Sprintf("actioncache://app:1985/blobs/ac/%s/%d", d.Hash, d.SizeBytes)
		req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/"+mode+"?bytestream_url="+url.QueryEscape(uri)+"&request_context="+url.QueryEscape(base64.StdEncoding.EncodeToString(field(nil, 2, []byte(f.GroupID)))), nil)
		if err != nil {
			return err
		}
		if mode == "api" {
			payload, err := json.Marshal(map[string]any{"uri": uri})
			if err != nil {
				return err
			}
			req, err = http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/GetFile", bytes.NewReader(payload))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("x-buildbuddy-api-key", f.APIKey)
		resp, err := f.Web.Client.Do(req)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != 404 {
			return fmt.Errorf("absent AC HTTP %s: %d %s", mode, resp.StatusCode, b)
		}
	}
	return nil
}
