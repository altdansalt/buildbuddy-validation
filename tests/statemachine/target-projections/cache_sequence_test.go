package projections_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// CAS is content-addressed: updates use new digests and repeated writes retain
// exact contents. Two distinct keys act as clients of the same organization.
func TestTwoClientArtifactSequence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	key := f.Web.CreateAPIKey(t, f.GroupID, "second-projection-client", []string{"CACHE_WRITE"})
	second := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	clients := []*harness.Clients{f.Clients(harness.App), second}
	var contents [][]byte
	for step, data := range [][]byte{[]byte("artifact-v1"), []byte("artifact-v2"), []byte("artifact-v3"), []byte("artifact-v1")} {
		started := time.Now()
		d, err := clients[step%2].Upload(ctx, "", data)
		require.NoError(t, err)
		require.Equal(t, harness.Digest(data).Hash, d.Hash)
		if step < 3 {
			contents = append(contents, data)
		}
		for stable := 0; stable < 2; stable++ {
			for _, content := range contents {
				digest := harness.Digest(content)
				for _, client := range clients {
					missing, err := client.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{digest}})
					require.NoError(t, err)
					require.Empty(t, missing.MissingBlobDigests)
					b, err := client.Read(ctx, "", digest)
					require.NoError(t, err)
					require.Equal(t, content, b)
					batch, err := client.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{digest}})
					require.NoError(t, err)
					require.Len(t, batch.Responses, 1)
					require.Equal(t, int32(0), batch.Responses[0].GetStatus().GetCode())
					require.Equal(t, content, batch.Responses[0].Data)
				}
			}
		}
		t.Logf("step=%d write/read/stability observation=%s", step, time.Since(started))
	}
}
