package bes_lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Isolated contract repro: authenticated downloads must map a deleted invocation
// to NotFound, just as detail and log RPCs do. See finding-deleted-download.md.
func TestDeletedDownloadNotFound(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := uuid.NewString()
	publishComplete(t, ctx, c, id)
	for _, artifact := range []string{"raw_json", "raw_proto", "buildlog"} {
		code, _ := auditDownload(t, ctx, f, id, artifact)
		require.Equal(t, 200, code)
	}
	_, err := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", "DeleteInvocation", map[string]any{"invocationId": id})
	require.NoError(t, err)
	for _, method := range []string{"GetInvocation", "GetEventLogChunk"} {
		req := map[string]any{"invocationId": id}
		if method == "GetInvocation" {
			req = map[string]any{"lookup": map[string]any{"invocationId": id}}
		}
		_, err := grpcRead(t, ctx, c, "buildbuddy.service.BuildBuddyService", method, req)
		require.Equal(t, codes.NotFound, status.Code(err))
	}
	for _, route := range []string{"download", "view"} {
		for _, artifact := range []string{"raw_json", "raw_proto", "buildlog"} {
			t.Run(route+"/"+artifact, func(t *testing.T) {
				start := time.Now()
				var code int
				var body []byte
				defer func() { t.Logf("deleted %s/%s after %s: HTTP %d %q", route, artifact, time.Since(start), code, body) }()
				// A two-second bounded observation rules out immediate convergence. Source
				// shows a deterministic DB-error mapping, rather than asynchronous cleanup.
				require.Eventually(t, func() bool { code, body = artifactFetch(t, ctx, f, id, artifact, route); return code == 404 }, 2*time.Second, 100*time.Millisecond, "deleted artifact should converge to HTTP 404")
			})
		}
	}
}
