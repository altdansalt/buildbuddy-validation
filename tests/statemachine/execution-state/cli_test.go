package executionstate_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func releasedCLI(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	// Released executable, pinned by content; never build BuildBuddy source.
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/buildbuddy-io/bazel/releases/download/5.0.492/bazel-5.0.492-linux-x86_64", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, 200, response.StatusCode)
	binary, err := io.ReadAll(io.LimitReader(response.Body, 100<<20))
	require.NoError(t, err)
	require.Equal(t, "e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9", fmt.Sprintf("%x", sha256.Sum256(binary)))
	dir := t.TempDir()
	path := filepath.Join(dir, "bb")
	require.NoError(t, os.WriteFile(path, binary, 0700))
	return path, dir
}
func downloadCLI(t *testing.T, ctx context.Context, f *harness.Fixture, d *repb.Digest, want []byte) {
	path, dir := releasedCLI(t, ctx)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		cmd := exec.CommandContext(ctx, path, "download", fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey)
		cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
		cmd.Dir = dir
		output, err := cmd.Output()
		if failure, ok := err.(*exec.ExitError); ok {
			t.Logf("bb download stderr: %s", failure.Stderr)
		}
		require.NoError(t, err)
		require.Equal(t, want, output)
	}
}

// Audit the execution-ID AC namespace used by bb execution get and the UI's
// independent /file/download path, rather than only inline GetExecution.
func auditCachedResponse(t *testing.T, ctx context.Context, f *harness.Fixture, instance, invocation, name string, want *repb.ExecuteResponse) {
	t.Helper()
	d := harness.Digest([]byte(name))
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		var cached *repb.ActionResult
		require.Eventually(t, func() bool {
			var err error
			cached, err = f.Clients(component).AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d})
			return err == nil
		}, 30*time.Second, 100*time.Millisecond)
		got := &repb.ExecuteResponse{}
		require.NoError(t, proto.Unmarshal(cached.StdoutRaw, got))
		require.True(t, responseEqual(want, got), "execution-ID cache differs: %s", component)
		t.Logf("AUDIT execution-ID AC %s agrees, status=%d", component, got.GetStatus().GetCode())
	}
	resource := fmt.Sprintf("actioncache://app:1985/%s/blobs/ac/%s/%d", instance, d.Hash, d.SizeBytes)
	params := url.Values{"bytestream_url": {resource}, "invocation_id": {invocation}, "request_context": {base64.StdEncoding.EncodeToString(field(2, []byte(f.GroupID)))}}
	req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/download?"+params.Encode(), nil)
	require.NoError(t, err)
	resp, err := f.Web.Client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", data)
	ar := &repb.ActionResult{}
	require.NoError(t, proto.Unmarshal(data, ar))
	got := &repb.ExecuteResponse{}
	require.NoError(t, proto.Unmarshal(ar.StdoutRaw, got))
	require.True(t, responseEqual(want, got), "UI file download differs")
	t.Logf("AUDIT HTTP execution-response download agrees, status=%d", got.GetStatus().GetCode())
}

func executionCLI(t *testing.T, ctx context.Context, f *harness.Fixture, name string, want *repb.ExecuteResponse) {
	path, dir := releasedCLI(t, ctx)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		cmd := exec.CommandContext(ctx, path, "execution", "get", name, "--output=json", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey)
		cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
		cmd.Dir = dir
		output, err := cmd.Output()
		if e, ok := err.(*exec.ExitError); ok {
			t.Logf("CLI stderr: %s", e.Stderr)
		}
		require.NoError(t, err)
		var document map[string]any
		require.NoError(t, json.Unmarshal(output, &document))
		if result, ok := document["result"].(map[string]any); ok {
			if md, ok := result["executionMetadata"].(map[string]any); ok {
				delete(md, "auxiliaryMetadata")
			}
		}
		output, err = json.Marshal(document)
		require.NoError(t, err)
		got := &repb.ExecuteResponse{}
		require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(output, got))
		require.True(t, responseEqual(want, got), "bb execution get %s", component)
	}
}
