package projections_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func checkAdditional(t *testing.T, f *harness.Fixture, m buildModel) {
	step := m.step
	for _, x := range m.targets {
		filtered := rpc(t, f, "GetTarget", map[string]any{"invocationId": m.id, "filter": x.label, "status": x.status})
		groups := objects(t, filtered, "targetGroups")
		require.Len(t, groups, 1)
		targets := objects(t, groups[0].(map[string]any), "targets")
		require.Len(t, targets, 1)
		require.Equal(t, x.label, targets[0].(map[string]any)["metadata"].(map[string]any)["label"])
	}
	empty := rpc(t, f, "GetTarget", map[string]any{"invocationId": m.id, "filter": "no-such-target"})
	for _, g := range objects(t, empty, "targetGroups") {
		require.Empty(t, objects(t, g.(map[string]any), "targets"))
	}

	code, res := api(t, f, "GetInvocation", map[string]any{"selector": map[string]any{"invocationId": m.id}, "includeMetadata": true})
	require.Equal(t, 200, code)
	invs := objects(t, res, "invocation")
	require.Len(t, invs, 1)
	inv := invs[0].(map[string]any)
	require.Equal(t, "COMPLETE_INVOCATION_STATUS", inv["invocationStatus"])
	require.Equal(t, step != 1, inv["success"] == true)
	require.Equal(t, fmt.Sprintf("%040d", step+1), inv["commitSha"])
	raw := readRPC(t, f, "/api.v1.ApiService/GetInvocation", field(1, str(1, m.id)))
	require.Len(t, values(raw, 1), 1)
	require.Equal(t, uint64(1), scalar(values(raw, 1)[0], 25))
	require.Equal(t, step != 1, scalar(values(raw, 1)[0], 3) == 1)
	for _, x := range m.targets {
		grouped := rpc(t, f, "GetTarget", map[string]any{"invocationId": m.id, "status": x.status})
		groups := objects(t, grouped, "targetGroups")
		require.Len(t, groups, 1)
		targets := objects(t, groups[0].(map[string]any), "targets")
		require.Len(t, targets, 1)
		require.Equal(t, x.label, targets[0].(map[string]any)["metadata"].(map[string]any)["label"])
		for attempt := 1; attempt <= x.attempts; attempt++ {
			content := artifactContent(m.id, x.label, attempt, x.testStatus == 4 || (x.attempts > 1 && attempt < x.attempts))
			d := harness.Digest(content)
			for _, c := range []harness.Component{harness.App, harness.CacheProxy} {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				missing, err := f.Clients(c).CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
				require.NoError(t, err)
				require.Empty(t, missing.MissingBlobDigests)
				batches, err := f.Clients(c).CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}})
				cancel()
				require.NoError(t, err)
				require.Len(t, batches.Responses, 1)
				require.Equal(t, int32(0), batches.Responses[0].GetStatus().GetCode())
				require.Equal(t, content, batches.Responses[0].Data)
			}
			uri := "bytestream://app:1985/" + harness.ResourceName("", d, false)
			require.Equal(t, content, streamedFile(t, f, uri))
			partial := readRPC(t, f, "/api.v1.ApiService/GetFileRange", msg(str(1, uri), num(2, 1), num(3, d.SizeBytes-1)))
			require.Equal(t, content[1:len(content)-1], values(partial, 1)[0])
			resource := msg(field(1, msg(str(1, d.Hash), num(2, d.SizeBytes))), num(4, 2))
			meta := rpc(t, f, "GetCacheMetadata", map[string]any{"resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "cacheType": "CAS"}})
			require.Equal(t, fmt.Sprint(d.SizeBytes), fmt.Sprint(meta["digestSizeBytes"]))
			metadataWire := readRPC(t, f, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", field(2, resource))
			require.Equal(t, uint64(d.SizeBytes), scalar(metadataWire, 5))
			rangeBytes := readRPC(t, f, "/api.v1.ApiService/GetFileRange", msg(str(1, uri), num(3, d.SizeBytes)))
			require.Equal(t, content, values(rangeBytes, 1)[0])
		}
	}
}

func checkLogsAndArtifacts(t *testing.T, f *harness.Fixture, m buildModel, binary, dir string) {
	t.Helper()
	expected := []byte("log/" + m.id + "\n")
	require.Equal(t, expected, webBytes(t, f, "/file/download?invocation_id="+m.id+"&artifact=buildlog&attempt=1"))
	chunk := readRPC(t, f, "/buildbuddy.service.BuildBuddyService/GetEventLogChunk", msg(str(2, m.id), num(4, 100), num(5, 1)))
	require.Equal(t, expected, values(chunk, 4)[0])
	ui := rpc(t, f, "GetEventLogChunk", map[string]any{"invocationId": m.id, "minLines": 100, "type": "BUILD_LOG"})
	require.Equal(t, base64.StdEncoding.EncodeToString(expected), ui["buffer"])
	require.Equal(t, append([]byte("===== BUILD LOGS =====\n"), expected...), runCLI(t, binary, dir, "view", m.id, "--target=grpc://"+f.Endpoint(harness.App).GRPC))
	code, res := api(t, f, "GetInvocation", map[string]any{"selector": map[string]any{"invocationId": m.id}, "includeArtifacts": true, "includeBuildToolLogs": true})
	require.Equal(t, 200, code)
	invocation := objects(t, res, "invocation")[0].(map[string]any)
	artifacts := objects(t, invocation, "artifacts")
	require.Len(t, artifacts, 1)
	require.Equal(t, "bundle.txt", artifacts[0].(map[string]any)["name"])
	logs := objects(t, invocation, "buildToolLogs")
	require.Len(t, logs, 1)
	require.Equal(t, "report.txt", logs[0].(map[string]any)["name"])
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("report/"+m.id)), logs[0].(map[string]any)["contents"])
	bd := harness.Digest(bundleContent(m.id))
	require.Equal(t, bundleContent(m.id), streamedFile(t, f, artifacts[0].(map[string]any)["uri"].(string)))
	for _, c := range []harness.Component{harness.App, harness.CacheProxy} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		got, err := f.Clients(c).Read(ctx, "", bd)
		cancel()
		require.NoError(t, err)
		require.Equal(t, bundleContent(m.id), got)
	}
	outDir := t.TempDir()
	runCLI(t, binary, dir, "download", "artifacts", m.id, "--target=grpc://"+f.Endpoint(harness.App).GRPC, "--api_key="+f.APIKey, "--output_directory="+outDir)
	b, err := os.ReadFile(filepath.Join(outDir, "bundle.txt"))
	require.NoError(t, err)
	require.Equal(t, bundleContent(m.id), b)
}
