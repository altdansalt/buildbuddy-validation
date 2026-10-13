package bes_test

import (
	"strings"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
)

func TestMixedIdentityPersistence(t *testing.T) {
	f := startApp(t)
	c := f.Clients(harness.App)
	control := uuid.NewString()
	checkACKs(t, c, request(control, 1, started()), request(control, 2, progress("control\n")), request(control, 3, finished()))
	if logs := eventLog(t, f, control); logs != "control\n" {
		t.Fatalf("control logs=%q", logs)
	}
	a, b := uuid.NewString(), uuid.NewString()
	const marker = "second-invocation-marker\n"
	acks, rpcErr := publish(t, c, request(a, 1, started()), request(b, 2, progress(marker)), request(b, 3, finished()))
	inv, readErr := invocation(f, a)
	var logs string
	if readErr == nil {
		logs = eventLog(t, f, a)
	}
	_, secondErr := invocation(f, b)
	t.Logf("mixed stream ACKs=%v RPC error=%v; first=%v read error=%v logs=%q; second error=%v", acks, rpcErr, inv, readErr, logs, secondErr)
	// Even if validation happens after creating A, events explicitly labeled B
	// must not contribute to A's persisted build log or mark A successful.
	if strings.Contains(logs, marker) || inv["success"] == true {
		t.Fatalf("invocation A persisted invocation B's progress/finish: success=%v logs=%q", inv["success"], logs)
	}
	if rpcErr == nil {
		t.Fatal("mixed-identity stream unexpectedly accepted")
	}
}
