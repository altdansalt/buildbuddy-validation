package casbatch

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"testing"
	"time"
)

func client(t *testing.T) (context.Context, *harness.Clients) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx, f.Clients(harness.App)
}
