package streams

import (
	"bytes"
	"context"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"sort"
	"testing"
	"time"
)

func TestStreamConvergence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	key2 := f.Web.CreateAPIKey(t, f.GroupID, "second-stream-client", []string{"CAS_WRITE", "CACHE_WRITE"})
	clients := []*harness.Clients{f.Clients(harness.App), harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key2)}
	// Each creation updates the reference model; every subsequent operation checks
	// all model entries, including earlier blobs, to catch representation collisions.
	model := map[string][]byte{}
	for writer, c := range clients {
		for _, compressed := range []bool{false, true} {
			instance := "streams/nested"
			b := bytes.Repeat([]byte(fmt.Sprintf("writer=%d compressed=%t; logical content\n", writer, compressed)), 40000)
			d := harness.Digest(b)
			absentCtx, absentCancel := context.WithTimeout(context.Background(), 20*time.Second)
			issues := absent(absentCtx, f, clients, instance, d)
			absentCancel()
			if len(issues) > 0 {
				t.Fatalf("initial absent state disagrees: %v", issues)
			}

			for repeat := 0; repeat < 3; repeat++ {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				representation := compressed
				if repeat == 2 {
					representation = !compressed
				}
				_, e := c.Write(ctx, instance, b, representation)
				cancel()
				if e != nil {
					t.Fatal(e)
				}
				model[d.Hash] = b
				hashes := make([]string, 0, len(model))
				for hash := range model {
					hashes = append(hashes, hash)
				}
				sort.Strings(hashes)
				for _, hash := range hashes {
					want := model[hash]
					started := time.Now()
					var firstError error
					deadline, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					ticker := time.NewTicker(100 * time.Millisecond)
					var last error
					for {
						last = check(deadline, f, clients, instance, &repb.Digest{Hash: hash, SizeBytes: int64(len(want))}, want)
						if last != nil && firstError == nil {
							firstError = last
						}
						if last == nil {
							t.Logf("writer=%d repeat=%d digest=%s convergence_upper_bound=%s", writer, repeat, hash, time.Since(started))
							time.Sleep(150 * time.Millisecond)
							if stable := check(deadline, f, clients, instance, &repb.Digest{Hash: hash, SizeBytes: int64(len(want))}, want); stable != nil {
								t.Fatalf("converged state unstable: %v", stable)
							}
							break
						}
						select {
						case <-deadline.Done():
							ticker.Stop()
							cancel()
							t.Fatalf("writer=%d zstd=%t repeat=%d digest=%s: %v", writer, compressed, repeat, hash, fmt.Errorf("first=%v last=%v", firstError, last))
						case <-ticker.C:
						}
					}
					ticker.Stop()
					cancel()
				}
			}
		}
	}
}
