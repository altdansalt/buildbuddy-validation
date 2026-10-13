package bes_test

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestInvalidSequenceCode(t *testing.T) {
	f := startApp(t)
	c := f.Clients(harness.App)
	control := uuid.NewString()
	checkACKs(t, c, request(control, 1, started()), request(control, 2, finished()))
	// UNKNOWN deliberately keeps malformed streams retryable; no event is ACKed.
	for _, seqs := range [][]int64{{1, 3}, {1, 1}} {
		id := uuid.NewString()
		acks, err := publish(t, c, request(id, seqs[0], started()), request(id, seqs[1], finished()))
		if len(acks) != 0 || status.Code(err) != codes.Unknown {
			t.Errorf("sequence=%v ACKs=%v code=%s error=%v; want Unknown without ACKs", seqs, acks, status.Code(err), err)
		}
	}
}
