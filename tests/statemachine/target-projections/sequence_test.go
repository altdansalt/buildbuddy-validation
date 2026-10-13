package projections_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ActionCache entries are mutable (not content addressed): every update through
// either component must become visible through both.
func TestActionCacheUpdateSequence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	key := f.Web.CreateAPIKey(t, f.GroupID, "second-ac-client", []string{"CACHE_WRITE"})
	second := harness.Dial(t, f.Endpoint(harness.CacheProxy).GRPC, key)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	d := harness.Digest([]byte("action-cache-update-sequence"))
	writers := []*harness.Clients{app, proxy, second, app, proxy, proxy}
	for step, w := range writers {
		want := fmt.Sprintf("exit=%d stdout=v%d", step+1, step+1)
		_, err := w.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: &repb.ActionResult{ExitCode: int32(step + 1), StdoutRaw: []byte(fmt.Sprintf("v%d", step+1))}})
		require.NoError(t, err)
		agree(t, fmt.Sprintf("ac step %d", step+1), 20*time.Second, 2*time.Second, func() string {
			for name, c := range map[string]*harness.Clients{"app": app, "proxy": proxy, "second-key": second} {
				if got := acState(ctx, c, d); got != want {
					return fmt.Sprintf("%s=%q want %q", name, got, want)
				}
			}
			return ""
		})
	}
}

// Write via one component, delete via the app, re-create via the other: while
// the proxy has not cached the blob, absence and presence must be seen everywhere.
func TestDeleteRecreateSequenceColdProxy(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	data := []byte("cold proxy delete sequence")
	d := harness.Digest(data)
	uri := harness.ResourceName("", d, false)
	both := func(present bool) func() string {
		return func() string {
			for name, c := range map[string]*harness.Clients{"app": app, "proxy": proxy} {
				if got := casState(ctx, f, c, d, uri, false); got != casWant(present, len(data)) {
					return fmt.Sprintf("%s=%s want %s", name, got, casWant(present, len(data)))
				}
			}
			return ""
		}
	}
	// 1. create via the app, never read through the proxy before the delete.
	_, err := app.Upload(ctx, "", data)
	require.NoError(t, err)
	agree(t, "create via app", 10*time.Second, time.Second, func() string {
		// presence at the app only, so the proxy stays cold
		if got := casState(ctx, f, app, d, uri, false); got != casWant(true, len(data)) {
			return got
		}
		return ""
	})
	// 2. delete, 3. repeat the delete (valid, idempotent), and absence everywhere.
	deleteViaApp(t, ctx, f, uri)
	agree(t, "delete via app", 10*time.Second, 2*time.Second, both(false))
	deleteViaApp(t, ctx, f, uri)
	agree(t, "repeat delete", 10*time.Second, time.Second, both(false))
	// 4. re-create via the proxy, readable through both.
	_, err = proxy.Upload(ctx, "", data)
	require.NoError(t, err)
	agree(t, "re-create via proxy", 10*time.Second, 2*time.Second, both(true))
}

// ActionCache deletion through DeleteFile with a cold proxy.
func TestActionCacheDeleteColdProxy(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	d := harness.Digest([]byte("ac-delete-cold"))
	_, err := app.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: &repb.ActionResult{ExitCode: 7}})
	require.NoError(t, err)
	agree(t, "ac create", 10*time.Second, time.Second, func() string {
		if got := acState(ctx, app, d); got != "exit=7 stdout=" {
			return got
		}
		return ""
	})
	deleteViaApp(t, ctx, f, "blobs/ac/"+d.Hash+"/"+fmt.Sprint(d.SizeBytes))
	agree(t, "ac delete", 10*time.Second, 2*time.Second, func() string {
		for name, c := range map[string]*harness.Clients{"app": app, "proxy": proxy} {
			if got := acState(ctx, c, d); got != "absent" {
				return name + "=" + got
			}
		}
		return ""
	})
}

// Control for the CAS divergence: an ActionCache entry that the proxy has already
// served is deleted at the app with the (non-default) DeleteFile API.
func TestActionCacheDeleteWarmProxy(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, proxy := f.Clients(harness.App), f.Clients(harness.CacheProxy)
	d := harness.Digest([]byte("ac-delete-warm"))
	_, err := app.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: &repb.ActionResult{ExitCode: 7}})
	require.NoError(t, err)
	require.Equal(t, "exit=7 stdout=", acState(ctx, proxy, d)) // warm the proxy
	deleteViaApp(t, ctx, f, "blobs/ac/"+d.Hash+"/"+fmt.Sprint(d.SizeBytes))
	require.Equal(t, "absent", acState(ctx, app, d))
	started := time.Now()
	agree(t, "ac delete warm proxy", 60*time.Second, time.Second, func() string {
		if got := acState(ctx, proxy, d); got != "absent" {
			return fmt.Sprintf("proxy=%s after %s", got, time.Since(started))
		}
		return ""
	})
}
