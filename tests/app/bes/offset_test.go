package bes_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"testing"
)

func TestFreshInvocationOffset(t *testing.T) {
	f := startApp(t)
	c := f.Clients(harness.App)
	checkACKs(t, c, request(uuid.NewString(), 1, started()))
	id := uuid.NewString()
	acks, err := publish(t, c, request(id, 2, started()))
	if err == nil || len(acks) > 0 {
		result, lookupErr := f.Web.RPC("GetInvocation", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "lookup": map[string]any{"invocationId": id}})
		t.Logf("ACKed invocation readback: result=%v error=%v", result, lookupErr)
		t.Fatalf("fresh stream starting at 2 must be rejected without ACKs: ACKs=%v error=%v", acks, err)
	}
}
