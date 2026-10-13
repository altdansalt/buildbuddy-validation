package isolation_test

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestInputMutationDoesNotPoisonLaterAction(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	require.Equal(t, "original\n", execute(t, c, `set -eux; cat input; printf 'poisoned\n' > input`, false, nil))
	// Verify the app CAS still contains the original input; only executor-local
	// state has been changed. A distinct command forces a distinct action digest.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	original, err := c.Read(ctx, "isolation-state", harness.Digest([]byte("original\n")))
	require.NoError(t, err)
	require.Equal(t, []byte("original\n"), original)
	actual := execute(t, c, `set -eux; printf 'next action: '; cat input`, false, nil)
	require.Equal(t, "next action: original\n", actual, "later action must materialize the bytes named by InputRootDigest")
}
