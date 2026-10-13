package metadata_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

// Namespaced CAS blobs mask a CLI namespace rewrite because Pebble deduplicates
// CAS; manifest keys include the namespace, exposing the altered resource name.
func TestCLINamespacedSplice(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	const instance = "cli-splice-model"
	left := bytes.Repeat([]byte("l"), 3<<20)
	right := bytes.Repeat([]byte("r"), 2<<20)
	ld, err := c.Upload(ctx, instance, left)
	require.NoError(t, err)
	rd, err := c.Upload(ctx, instance, right)
	require.NoError(t, err)
	data := append(left, right...)
	d := harness.Digest(data)
	_, err = c.CAS.SpliceBlob(ctx, &repb.SpliceBlobRequest{InstanceName: instance, BlobDigest: d, ChunkDigests: []*repb.Digest{ld, rd}})
	require.NoError(t, err)
	bb, err := exec.LookPath("bb")
	require.NoError(t, err)
	versionCmd := exec.CommandContext(ctx, bb, "version", "--cli")
	versionCmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+t.TempDir(), "USE_BAZEL_VERSION=8.4.2")
	version, err := versionCmd.CombinedOutput()
	t.Logf("CLI version query: %s (error=%v)", version, err)
	binary, err := os.ReadFile(bb)
	require.NoError(t, err)
	t.Logf("CLI binary sha256: %x", sha256.Sum256(binary))
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			reader := f.Clients(component)
			got, err := reader.Read(ctx, instance, d)
			require.NoError(t, err)
			require.Equal(t, data, got)
			// CLI source documents /foo/blobs/HASH/SIZE as a valid namespace form.
			started := time.Now()
			for attempt := 0; attempt < 11; attempt++ {
				cmd := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "/"+harness.ResourceName(instance, d, false))
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				got, err := cmd.Output()
				if err == nil {
					require.Equal(t, data, got)
					break
				}
				t.Logf("%s attempt=%d elapsed=%s error=%v stderr=%s", component, attempt, time.Since(started), err, stderr.String())
				if attempt == 10 {
					require.NoError(t, err, stderr.String())
				}
				time.Sleep(time.Second)
			}
		})
	}
}
