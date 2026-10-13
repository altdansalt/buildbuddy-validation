package bes_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"testing"
)

func TestChangedStreamIdentity(t *testing.T) {
	f := startApp(t)
	c := f.Clients(harness.App)
	checkACKs(t, c, request(uuid.NewString(), 1, started()))
	first, second := uuid.NewString(), uuid.NewString()
	acks, err := publish(t, c, request(first, 1, started()), request(second, 2, finished()))
	if err == nil {
		t.Fatalf("RPC containing distinct streams must be rejected: sent invocation IDs=[%s,%s] ACKs=%v error=%v", first, second, acks, err)
	}
}
