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

// agree polls observe until it returns "" (no mismatch) within bound, reports
// the convergence time, then requires the state to stay stable for hold.
func agree(t *testing.T, name string, bound, hold time.Duration, observe func() string) {
	t.Helper()
	started := time.Now()
	var last string
	deadline := started.Add(bound)
	for {
		if last = observe(); last == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no agreement within %s: %s", name, bound, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("%s: converged after %s", name, time.Since(started))
	for end := time.Now().Add(hold); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if got := observe(); got != "" {
			t.Fatalf("%s: converged state not stable: %s", name, got)
		}
	}
}

func acState(ctx context.Context, c *harness.Clients, d *repb.Digest) string {
	r, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
	if status.Code(err) == codes.NotFound {
		return "absent"
	}
	if err != nil {
		return "error:" + err.Error()
	}
	return fmt.Sprintf("exit=%d stdout=%s", r.ExitCode, r.StdoutRaw)
}

func deleteViaApp(t *testing.T, ctx context.Context, f *harness.Fixture, uri string) {
	t.Helper()
	req := str(1, uri)
	var out []byte
	require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, "/api.v1.ApiService/DeleteFile", &req, &out, grpc.ForceCodec(wireCodec{})))
}

// This codec transports the released schema directly, without building the app.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}

// casState lists what every CAS reader reports for d.
func casState(ctx context.Context, f *harness.Fixture, c *harness.Clients, d *repb.Digest, uri string, withAppOnly bool) string {
	var parts []string
	m, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{d}})
	if err != nil {
		return "fmb-error:" + err.Error()
	}
	parts = append(parts, fmt.Sprintf("missing=%d", len(m.MissingBlobDigests)))
	b, err := c.Read(ctx, "", d)
	parts = append(parts, fmt.Sprintf("read=%d/%v", len(b), status.Code(err)))
	br, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}})
	if err != nil {
		return "batch-error:" + err.Error()
	}
	parts = append(parts, fmt.Sprintf("batch=%d/%d", len(br.Responses[0].Data), br.Responses[0].GetStatus().GetCode()))
	return fmt.Sprint(parts)
}

func casWant(present bool, size int) string {
	if present {
		return fmt.Sprintf("[missing=0 read=%d/OK batch=%d/0]", size, size)
	}
	return fmt.Sprintf("[missing=1 read=0/NotFound batch=0/%d]", int32(codes.NotFound))
}
