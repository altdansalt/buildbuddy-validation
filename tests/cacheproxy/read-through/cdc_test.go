package readthrough_test

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
)

func TestCDCSeededReadThrough(t *testing.T) {
	app, writer, reader, ctx := cdcFixture(t)
	wc, rc := writer.Clients(harness.CacheProxy), reader.Clients(harness.CacheProxy)
	rng := rand.New(rand.NewSource(20261013))
	t.Log("CDC replay seed: 20261013")
	for caseID := 0; caseID < 3; caseID++ {
		data := make([]byte, (1<<20)+rng.Intn(1<<20))
		rng.Read(data)
		d, err := wc.Write(ctx, instance, data, caseID%2 == 1)
		require.NoError(t, err)
		for pass := 0; pass < 2; pass++ {
			got, err := rc.Read(ctx, instance, d)
			require.NoError(t, err)
			require.Equal(t, data, got)
			offset := int64(rng.Intn(len(data)))
			got, err = read(ctx, rc, d, offset, 0, false)
			require.NoError(t, err)
			require.Equal(t, data[offset:], got)
			compressed, err := read(ctx, rc, d, offset, 0, true)
			require.NoError(t, err)
			got, err = harness.Decompress(compressed)
			require.NoError(t, err)
			require.Equal(t, data[offset:], got)
		}
	}
	// Prove enabled paths actually ran, rather than accepting ordinary-read fallback.
	checkMetric := func(f *harness.Fixture, needle string) {
		out, err := fixtureDocker(t, ctx, app.Endpoint(harness.App).Container)("port", f.Endpoint(harness.CacheProxy).Container, "9090")
		require.NoError(t, err, string(out))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+strings.TrimSpace(string(out))+"/metrics", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)
		found := false
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, needle+"{") {
				fields := strings.Fields(line)
				v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
				require.NoError(t, err)
				if v > 0 {
					found = true
					t.Log(line)
				}
			}
		}
		require.True(t, found, fmt.Sprintf("no positive metric %s; CDC path not exercised", needle))
	}
	checkMetric(writer, "buildbuddy_proxy_byte_stream_chunked_write_chunks_total")
	checkMetric(reader, "buildbuddy_proxy_byte_stream_chunked_read_chunks_remote")
}
