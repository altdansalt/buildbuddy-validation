package membership

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Use a fresh digest for every step and writer so a read-only no-op cannot
// accidentally pass by reading data stored during an earlier privileged step.
func checkCacheWrites(t *testing.T, member *identity, oracle *harness.Clients, m model) {
	t.Helper()
	if member.targetKey == "" {
		return
	}
	member.writeSequence++
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for i, c := range member.cacheClients {
		payload := []byte(fmt.Sprintf("membership write %d via %d", member.writeSequence, i))
		d := harness.Digest(payload)
		_, casErr := c.Upload(ctx, "", payload)
		_, acErr := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: d, ActionResult: &repb.ActionResult{ExitCode: int32(member.writeSequence)}})
		if m.status != 1 {
			for _, err := range []error{casErr, acErr} {
				if status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
					t.Fatalf("removed key writes: %v", err)
				}
			}
			continue
		}
		if casErr != nil || acErr != nil {
			t.Fatalf("cache %d writes CAS=%v AC=%v", i, casErr, acErr)
		}
		// Read-only writes deliberately report OK but must not change stored state.
		_, readErr := oracle.Read(ctx, "", d)
		if m.role == 8 {
			if status.Code(readErr) != codes.NotFound {
				t.Fatalf("reader CAS write changed app state: %v", readErr)
			}
		} else if readErr != nil {
			t.Fatalf("CAS write not observable via app: %v", readErr)
		}
		ac, readErr := oracle.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
		if m.role == 1 || m.role == 8 {
			if status.Code(readErr) != codes.NotFound {
				t.Fatalf("readonly AC write changed app state: %v", readErr)
			}
		} else if readErr != nil || ac.ExitCode != int32(member.writeSequence) {
			t.Fatalf("AC write not observable via app: %v %v", ac, readErr)
		}
		for j, reader := range member.cacheClients {
			data, e := reader.Read(ctx, "", d)
			if m.role == 8 {
				if status.Code(e) != codes.NotFound {
					t.Fatalf("reader CAS write changed cache %d: %v", j, e)
				}
			} else if e != nil || string(data) != string(payload) {
				t.Fatalf("CAS write not observable via cache %d: %v", j, e)
			}
			ac, e := reader.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: d})
			if m.role == 1 || m.role == 8 {
				if status.Code(e) != codes.NotFound {
					t.Fatalf("readonly AC write changed cache %d: %v", j, e)
				}
			} else if e != nil || ac.ExitCode != int32(member.writeSequence) {
				t.Fatalf("AC write not observable via cache %d: %v", j, e)
			}
		}
	}
}
