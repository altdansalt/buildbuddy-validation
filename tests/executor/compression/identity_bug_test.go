package compression_test

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

// Shrunk from the seeded matrix to one file just above the input batch limit.
// Both client flags are false, eliminating an explicitly requested compressor
// as the explanation for contacting a server which does not support zstd.
func TestIdentityOnlyLargeInput(t *testing.T) {
	t.Log("deterministic reproduction seed=314159 input_size=2097153")
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{
		harness.App:      {"cache": harness.Config{"zstd_transcoding_enabled": false}},
		harness.Executor: {"cache": harness.Config{"client": harness.Config{"enable_upload_compression": false, "enable_download_compression": false}}},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	runFiles(t, ctx, c, c, rand.New(rand.NewSource(314159)), "identity-minimal", 0, false, []int{(2 << 20) + 1}, []bool{false})
}

// The smallest compressible output at the release's upload threshold. Inputs
// fit in BatchReadBlobs, so the large-file download bug cannot mask this path.
func TestIdentityOnlySmallOutput(t *testing.T) {
	t.Log("deterministic reproduction seed=314159 output_size=100")
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}, Config: map[harness.Component]harness.Config{
		harness.App:      {"cache": harness.Config{"zstd_transcoding_enabled": false}},
		harness.Executor: {"cache": harness.Config{"client": harness.Config{"enable_upload_compression": true, "enable_download_compression": false}}},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := f.Clients(harness.App)
	runFiles(t, ctx, c, c, rand.New(rand.NewSource(314159)), "identity-output-minimal", 0, false, []int{100}, []bool{false})
}
