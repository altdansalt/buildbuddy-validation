package streams

import (
	"bytes"
	"context"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"strings"
	"testing"
	"time"
)

// Opt-in delete API: warm both caches, delete at app, require absence at both,
// then re-create via the opposite endpoint. A warmed proxy may retain CAS data.
func TestDeleteRecreateConvergence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--enable_cache_delete_api=true"}}})
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	want := bytes.Repeat([]byte("delete and recreate\n"), 40000)
	d := harness.Digest(want)
	for step := 0; step < 2; step++ {
		if _, err := clients[step%2].Write(ctx, "delete", want, step%2 == 1); err != nil {
			t.Fatal(err)
		}
		if err := check(ctx, f, clients, "delete", d, want); err != nil {
			t.Fatal(err)
		}
		if err := clients[0].Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", wrapperspb.String(harness.ResourceName("delete", d, false)), new(emptypb.Empty)); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		var last error
		for {
			issues := absent(ctx, f, clients, "delete", d)
			last = nil
			if len(issues) > 0 {
				last = fmt.Errorf("%s", strings.Join(issues, "; "))
			}

			if last == nil {
				t.Logf("delete convergence=%s", time.Since(started))
				time.Sleep(150 * time.Millisecond)
				if issues := absent(ctx, f, clients, "delete", d); len(issues) > 0 {
					t.Errorf("unstable deletion: %v", issues)
				}
				break
			}
			if time.Since(started) > 30*time.Second {
				t.Errorf("delete did not converge within %s: %v", time.Since(started), last)
				break
			}
			time.Sleep(time.Second)
		}
	}
	if _, err := clients[0].Write(ctx, "delete", want, false); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if pass > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		if err := check(ctx, f, clients, "delete", d, want); err != nil {
			t.Fatal(err)
		}
	}

}
