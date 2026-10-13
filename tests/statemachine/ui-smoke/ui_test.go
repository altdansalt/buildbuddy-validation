package uismoke

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

// These minimal wire messages follow released v2.314.0 proto/api_key.proto and
// proto/context.proto. The repo has no generated BuildBuddy service bindings.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func field(n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), v)
}
func bytesField(t *testing.T, b []byte, want protowire.Number) []byte {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		require.Positive(t, k)
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			require.Positive(t, k)
			if n == want {
				return v
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			require.Positive(t, k)
			b = b[k:]
		}
	}
	t.Fatalf("response missing field %d", want)
	return nil
}

func TestGRPCMutationVisibleInUI(t *testing.T) {
	// Chromium treats the default hostname "app" as an HSTS-preloaded TLD.
	// Use a reserved .test hostname for this HTTP-only fixture and its cookies.
	f := harness.Start(t, harness.Options{
		Components: []harness.Component{harness.App},
		Config: map[harness.Component]harness.Config{
			harness.App: {"app": harness.Config{"build_buddy_url": "http://buildbuddy.test:8080"}},
		},
		DockerArgs: map[harness.Component][]string{
			harness.App: {"--network-alias", "buildbuddy.test"},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	admin := f.Web.CreateAPIKey(t, f.GroupID, "ui-smoke-admin", []string{"ORG_ADMIN"})
	conn := harness.Dial(t, f.Endpoint(harness.App).GRPC, admin)
	requestContext := field(1, field(2, []byte(f.GroupID)))
	label := "grpc-created-ui-conformance-key"
	request := append(append([]byte(nil), requestContext...), field(3, []byte(label))...)
	request = append(request, protowire.AppendVarint(protowire.AppendTag(nil, 4, protowire.VarintType), 1)...)
	var response []byte
	require.NoError(t, conn.Conn.Invoke(harness.WithAPIKey(ctx, admin), "/buildbuddy.service.BuildBuddyService/CreateApiKey", request, &response, grpc.ForceCodec(wireCodec{})))
	id := bytesField(t, bytesField(t, response, 2), 1)
	browser := f.Browser(ctx, f.Web, f.GroupID)
	const path = "/settings/org/api-keys"
	const ready = ".api-keys-list"
	require.NoError(t, browser.Open(path, ready))
	text, err := browser.Text(ready)
	require.NoError(t, err)
	require.Contains(t, text, label)
	request = append(append([]byte(nil), requestContext...), field(2, id)...)
	require.NoError(t, conn.Conn.Invoke(harness.WithAPIKey(ctx, admin), "/buildbuddy.service.BuildBuddyService/DeleteApiKey", request, &response, grpc.ForceCodec(wireCodec{})))
	// A fresh navigation triggers the page's normal data load. This tests rendered
	// UI convergence after refresh, rather than promising server push updates.
	require.NoError(t, browser.Open(path, ready))
	text, err = browser.Text(ready)
	require.NoError(t, err)
	require.NotContains(t, text, label)
	require.Contains(t, text, "ui-smoke-admin") // Prove the list loaded after deletion.
}
