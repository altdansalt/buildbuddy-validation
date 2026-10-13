package snapshot_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
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
	proxy := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, adminKey)
	model := map[string]string{}
	instances := []string{"snapshot-repo-a", "snapshot-repo-b"}
	for _, i := range instances {
		for _, c := range []*harness.Clients{app, proxy} {
			_, e := version(ctx, c, i)
			require.Equal(t, codes.NotFound, status.Code(e))
		}
	}
	// Seed both repositories, then repeatedly invalidate A including a fallback ref.
	for step, idx := range []int{0, 1, 0, 0, 0} {
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
		require.NotEmpty(t, v)
		require.NotEqual(t, model[i], v, "repeat invalidation must advance version")
		model[i] = v
		for name, want := range model {
			require.Eventually(t, func() bool {
				a, ea := version(ctx, app, name)
				p, ep := version(ctx, proxy, name)
				return ea == nil && ep == nil && a == want && p == want
			}, 10*time.Second, 100*time.Millisecond, "step %d instance %s", step, name)
		}
	}
	// Valid URLs for unlinked repositories are accepted no-ops. These do not prove
	// linked workflow instance suffix transitions (which require GitHub auth).
	for _, url := range []string{"https://github.com/fixture/repo-a", "https://github.com/fixture/repo-b", "https://github.com/fixture/repo-a"} {
		require.NoError(t, app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/InvalidateAllSnapshotsForRepo", request(field(2, url)), &emptypb.Empty{}))
		_, e := f.Web.RPC("InvalidateAllSnapshotsForRepo", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "repoUrl": url})
		require.NoError(t, e)
		for name, want := range model {
			for _, c := range []*harness.Clients{app, proxy} {
				v, e := version(ctx, c, name)
				require.NoError(t, e)
				require.Equal(t, want, v)
			}
		}
	}
	e := app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetAuditLogs", &emptypb.Empty{}, &emptypb.Empty{})
	require.Equal(t, codes.Unimplemented, status.Code(e), "default fixture has no OLAP audit logger")
	_, e = f.Web.RPC("GetAuditLogs", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
	require.Error(t, e)
	require.True(t, strings.Contains(e.Error(), "Audit logger not configured"), "%v", e)
}
