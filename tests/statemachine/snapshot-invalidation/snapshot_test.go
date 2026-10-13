package snapshot_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func field(n protowire.Number, s string) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), []byte(s))
}
func hash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

// Released snaploader.SnapshotVersionKey: SHA256 of concatenated SHA256 strings.
func versionDigest(instance string) *repb.Digest {
	return &repb.Digest{Hash: hash(hash(instance) + hash(hash("platform")) + hash(hash("configuration")) + hash(".version")), SizeBytes: 1}
}
func request(b []byte) *emptypb.Empty {
	m := &emptypb.Empty{}
	m.ProtoReflect().SetUnknown(b)
	return m
}
func version(ctx context.Context, c *harness.Clients, instance string) (string, error) {
	r, e := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: versionDigest(instance), DigestFunction: repb.DigestFunction_BLAKE3})
	if e != nil {
		return "", e
	}
	for _, a := range r.GetExecutionMetadata().GetAuxiliaryMetadata() {
		if a.GetTypeUrl() != "type.googleapis.com/firecracker.SnapshotVersionMetadata" {
			continue
		}
		b := a.GetValue()
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 || n != 1 || typ != protowire.BytesType {
			return "", fmt.Errorf("invalid version metadata")
		}
		v, k := protowire.ConsumeString(b[k:])
		if k < 0 || v == "" {
			return "", fmt.Errorf("empty version")
		}
		return v, nil
	}
	return "", fmt.Errorf("missing version metadata")
}

func TestInvalidationTransitions(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adminKey := f.Web.CreateAPIKey(t, f.GroupID, "snapshot-admin", []string{"ORG_ADMIN", "CACHE_WRITE", "CAS_WRITE"})
	app := harness.Dial(t, f.Endpoint(harness.App).GRPC, adminKey)
	readerKey := f.Web.CreateAPIKey(t, f.GroupID, "snapshot-reader", []string{"CACHE_WRITE", "CAS_WRITE"})
	proxy := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, readerKey)
	model := map[string]string{}
	seen := map[string]bool{}
	instances := []string{"snapshot-repo-a", "snapshot-repo-b"}
	for _, i := range instances {
		for _, c := range []*harness.Clients{app, proxy} {
			_, e := version(ctx, c, i)
			require.Equal(t, codes.NotFound, status.Code(e))
		}
	}
	// Seed both repositories, then repeatedly invalidate A including a fallback ref.
	for step, idx := range []int{0, 1, 0, 0, 0, 1, 0, 1} {
		i := instances[idx]
		ref := "refs/heads/main"
		if step == 3 {
			ref = "refs/heads/feature"
		}
		if step%2 == 0 {
			key := append(field(1, i), field(2, hash("platform"))...)
			key = append(key, field(3, hash("configuration"))...)
			key = append(key, field(5, ref)...)
			require.NoError(t, app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/InvalidateSnapshot", request(field(2, string(key))), &emptypb.Empty{}))
		} else {
			_, e := f.Web.RPC("InvalidateSnapshot", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "snapshotKey": map[string]any{"instanceName": i, "platformHash": hash("platform"), "configurationHash": hash("configuration"), "ref": ref}})
			require.NoError(t, e)
		}
		v, e := version(ctx, app, i)
		require.NoError(t, e)
		require.Len(t, v, 10, "released snapshot version format")
		require.False(t, seen[v], "every mutation must produce a fresh version")
		seen[v] = true
		require.NotEqual(t, model[i], v, "repeat invalidation must advance version")

		model[i] = v
		started := time.Now()
		for name, want := range model {
			require.Eventually(t, func() bool {
				a, ea := version(ctx, app, name)
				p, ep := version(ctx, proxy, name)
				return ea == nil && ep == nil && a == want && p == want
			}, 10*time.Second, 100*time.Millisecond, "step %d instance %s", step, name)
		}
		t.Logf("step %d convergence: %s", step, time.Since(started))
		checkSurfaces(t, ctx, f, app, proxy, adminKey, model)
		time.Sleep(200 * time.Millisecond)
		checkSurfaces(t, ctx, f, app, proxy, adminKey, model)

	}
	// Valid URLs for unlinked repositories are accepted no-ops. These do not prove
	// linked workflow instance suffix transitions (which require GitHub auth).
	for _, url := range []string{"https://github.com/fixture/repo-a", "https://github.com/fixture/repo-b", "https://github.com/fixture/repo-a"} {
		require.NoError(t, app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/InvalidateAllSnapshotsForRepo", request(field(2, url)), &emptypb.Empty{}))
		checkSurfaces(t, ctx, f, app, proxy, adminKey, model)
		_, e := f.Web.RPC("InvalidateAllSnapshotsForRepo", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "repoUrl": url})
		require.NoError(t, e)
		checkSurfaces(t, ctx, f, app, proxy, adminKey, model)
		time.Sleep(200 * time.Millisecond)
		checkSurfaces(t, ctx, f, app, proxy, adminKey, model)
		for name, want := range model {
			for _, c := range []*harness.Clients{app, proxy} {
				v, e := version(ctx, c, name)
				require.NoError(t, e)
				require.Equal(t, want, v)
			}
		}
	}
	checkSurfaces(t, ctx, f, app, proxy, adminKey, model)
	time.Sleep(200 * time.Millisecond)
	checkSurfaces(t, ctx, f, app, proxy, adminKey, model)
	e := app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetAuditLogs", &emptypb.Empty{}, &emptypb.Empty{})
	require.Equal(t, codes.Unimplemented, status.Code(e), "default fixture has no OLAP audit logger")
	_, e = f.Web.RPC("GetAuditLogs", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
	require.Error(t, e)
	require.True(t, strings.Contains(e.Error(), "Audit logger not configured"), "%v", e)
}

// Metadata describes storage, not the random version; compare stored size to
// the serialized ActionResult and the UI response, excluding access timestamps.
func checkSurfaces(t *testing.T, ctx context.Context, f *harness.Fixture, app, proxy *harness.Clients, adminKey string, model map[string]string) {
	t.Helper()
	for i, v := range model {
		for _, c := range []*harness.Clients{app, proxy} {
			got, err := version(ctx, c, i)
			require.NoError(t, err)
			require.Equal(t, v, got)
		}

		for _, path := range []string{"/file/download", "/file/view"} {
			for _, target := range []string{"app:1985", "cacheproxy:1985"} {
				resourceURL := fmt.Sprintf("actioncache://%s/%s/blobs/ac/blake3/%s/1", target, i, versionDigest(i).Hash)
				req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+path+"?bytestream_url="+url.QueryEscape(resourceURL), nil)
				require.NoError(t, err)
				req.Header.Set("x-buildbuddy-api-key", adminKey)
				rsp, err := f.Web.Client.Do(req)
				require.NoError(t, err)
				body, err := io.ReadAll(rsp.Body)
				rsp.Body.Close()
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, rsp.StatusCode, "target %s: %s", target, body)
				ar := &repb.ActionResult{}
				require.NoError(t, proto.Unmarshal(body, ar))
				found := false
				for _, a := range ar.GetExecutionMetadata().GetAuxiliaryMetadata() {
					if a.GetTypeUrl() == "type.googleapis.com/firecracker.SnapshotVersionMetadata" {
						require.Equal(t, field(1, v), a.GetValue(), "HTTP version from %s instance %s", target, i)
						found = true
					}
				}
				require.True(t, found)
				t.Logf("instance %s: HTTP read via %s agrees with gRPC version %s", i, target, v)
			}
		}
		md, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": versionDigest(i).Hash, "sizeBytes": "1"}, "instanceName": i, "cacheType": "AC", "digestFunction": "BLAKE3"}})
		require.NoError(t, err)
		require.NotEmpty(t, md["storedSizeBytes"])
		require.NotEqual(t, "0", fmt.Sprint(md["storedSizeBytes"]))
		t.Logf("UI GetCacheMetadata: %v", md)
		resource := field(1, string(protowire.AppendVarint(protowire.AppendTag(field(1, versionDigest(i).Hash), 2, protowire.VarintType), 1)))
		resource = append(resource, field(2, i)...)
		resource = protowire.AppendVarint(protowire.AppendTag(resource, 4, protowire.VarintType), 1)
		resource = protowire.AppendVarint(protowire.AppendTag(resource, 5, protowire.VarintType), uint64(repb.DigestFunction_BLAKE3))
		response := &emptypb.Empty{}
		require.NoError(t, app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", request(field(2, string(resource))), response))
		fields := response.ProtoReflect().GetUnknown()
		var stored uint64
		for len(fields) > 0 {
			n, typ, k := protowire.ConsumeTag(fields)
			require.Greater(t, k, 0)
			fields = fields[k:]
			if typ == protowire.VarintType {
				value, k := protowire.ConsumeVarint(fields)
				require.Greater(t, k, 0)
				if n == 2 {
					stored = value
				}
				fields = fields[k:]
			} else {
				k := protowire.ConsumeFieldValue(n, typ, fields)
				require.Greater(t, k, 0)
				fields = fields[k:]
			}
		}
		require.Equal(t, fmt.Sprint(stored), fmt.Sprint(md["storedSizeBytes"]))
		ar, err := app.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: i, ActionDigest: versionDigest(i), DigestFunction: repb.DigestFunction_BLAKE3})
		require.NoError(t, err)
		require.Equal(t, uint64(proto.Size(ar)), stored)
	}
}
