package bes_lifecycle_test

import (
	"context"
	"encoding/base64"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Default chunk/page selectors should expose the most recent log, including
// its live buffered suffix. Explicit chunk 0000 does, but default selectors do not.
func TestLiveDefaultTail(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := uuid.NewString()
	requests := fixtureRequests(t, id, true)
	s, e := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, e)
	for _, r := range requests[:4] {
		require.NoError(t, s.Send(r))
	}
	want := "state-machine console output\n"
	req := map[string]any{"invocationId": id, "chunkId": "0000"}
	require.Eventually(t, func() bool {
		r, e := f.Web.RPC("GetEventLogChunk", req)
		return e == nil && r["buffer"] == base64.StdEncoding.EncodeToString([]byte(want))
	}, 20*time.Second, 100*time.Millisecond)
	explicit := rpc(t, f, "GetEventLogChunk", req)
	require.Equal(t, true, explicit["live"])
	// Fetch the pinned CLI before starting the bounded convergence observation.
	_, e = cliOutput(t, ctx, f, id)
	require.NoError(t, e)
	rootTest := t
	start := time.Now()
	t.Run("UI-default-chunk", func(t *testing.T) {
		r := rpc(t, f, "GetEventLogChunk", map[string]any{"invocationId": id})
		t.Logf("explicit=%v default=%v", explicit, r)
		got, _ := r["buffer"].(string)
		require.Equal(t, explicit["buffer"].(string), got)
	})
	t.Run("public-default-page", func(t *testing.T) {
		code, r := api(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": id}})
		require.Equal(t, 200, code)
		t.Logf("default GetLog=%v", r)
		log, _ := r["log"].(map[string]any)
		got, _ := log["contents"].(string)
		require.Equal(t, want, got)
	})
	for _, service := range []string{"buildbuddy.service.BuildBuddyService", "api.v1.ApiService"} {
		t.Run(service+"-default", func(t *testing.T) {
			method := "GetEventLogChunk"
			req := map[string]any{"invocationId": id}
			if service == "api.v1.ApiService" {
				method = "GetLog"
				req = map[string]any{"selector": map[string]any{"invocationId": id}}
			}
			r, e := grpcRead(t, ctx, c, service, method, req)
			require.NoError(t, e)
			t.Logf("default %s: %v", method, r)
			if method == "GetLog" {
				log, _ := r["log"].(map[string]any)
				got, _ := log["contents"].(string)
				require.Equal(t, want, got)
			} else {
				got, _ := r["buffer"].(string)
				require.Equal(t, explicit["buffer"].(string), got)
			}
		})
	}
	t.Run("CLI-bounded-convergence", func(t *testing.T) {
		var out []byte
		defer func() { t.Logf("CLI live after %s: %q", time.Since(start), out) }()
		require.Eventually(t, func() bool {
			var e error
			out, e = cliOutput(rootTest, ctx, f, id)
			require.NoError(t, e)
			return string(out) == "===== BUILD LOGS =====\n"+want
		}, 2*time.Second, 200*time.Millisecond)
	})
	// Complete the valid stream even after subtest assertion failures, then prove
	// this divergence resolves with finalization rather than idle polling.
	for _, r := range requests[4:] {
		require.NoError(t, s.Send(r))
	}
	require.NoError(t, s.CloseSend())
	for range requests {
		_, e = s.Recv()
		require.NoError(t, e)
	}
	_, e = s.Recv()
	require.Equal(t, io.EOF, e)
	checkCLI(t, ctx, f, id, want)
	t.Logf("default-tail omission resolved after completion; observed divergence window %s", time.Since(start))
}
