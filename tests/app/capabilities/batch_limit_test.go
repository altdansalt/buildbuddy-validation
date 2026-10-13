package capabilities_test

import (
	"bytes"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
)

func TestAdvertisedBatchReadLimit(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		t.Run(string(component), func(t *testing.T) {
			ctx := bounded(t)
			c := f.Clients(component)
			caps, err := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{InstanceName: "limit-repro"})
			require.NoError(t, err)
			limit := caps.GetCacheCapabilities().GetMaxBatchTotalSizeBytes()
			require.GreaterOrEqual(t, limit, int64(0))
			size := int64(4000001)
			if limit > 0 && limit < size {
				size = limit
			}
			t.Logf("advertised limit=%d; testing %d bytes", limit, size)
			// Below gRPC's receive limit and just above the hidden 4,000,000-byte
			// application read limit. The BatchRead request itself is tiny.
			data := bytes.Repeat([]byte("x"), int(size))
			d, err := c.Upload(ctx, "limit-repro", data)
			require.NoError(t, err, "same-sized BatchUpdate succeeds")
			got, err := c.Read(ctx, "limit-repro", d)
			require.NoError(t, err)
			require.Equal(t, data, got, "ByteStream control proves the blob exists")
			read, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: "limit-repro", DigestFunction: repb.DigestFunction_SHA256, Digests: []*repb.Digest{d}})
			require.NoError(t, err, "batch read within advertised limit %d must succeed", limit)
			require.Len(t, read.GetResponses(), 1)
			require.Zero(t, read.Responses[0].GetStatus().GetCode())
			require.Equal(t, data, read.Responses[0].Data)
		})
	}
}
