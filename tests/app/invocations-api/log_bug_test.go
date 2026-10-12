package invocations_api_test

import (
	"encoding/base64"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestGetLogStartsAtFirstChunk(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	// Default log chunk size is 2,000,000 bytes. Multiple small progress events
	// cross that boundary without exceeding the BES per-message size limit.
	logs := []string{"FIRST-LINE\n"}
	for i := 0; i < 25; i++ {
		logs = append(logs, strings.Repeat("log line\n", 10000))
	}
	logs = append(logs, "LAST-LINE\n")
	id := upload(t, f, logs, 0)
	first := rpc(t, f, "GetEventLogChunk", map[string]any{"invocationId": id, "chunkId": "0000"})
	decoded, err := base64.StdEncoding.DecodeString(first["buffer"].(string))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(decoded), "FIRST-LINE\n"))
	require.NotEmpty(t, first["nextChunkId"], "must have multiple chunks to exercise pagination")
	var got strings.Builder
	token := ""
	for n := 0; n < 10; n++ {
		code, res := api(t, f, "GetLog", map[string]any{"selector": map[string]any{"invocationId": id}, "pageToken": token})
		require.Equal(t, 200, code)
		got.WriteString(res["log"].(map[string]any)["contents"].(string))
		token, _ = res["nextPageToken"].(string)
		if token == "" {
			break
		}
	}
	expected := strings.Join(logs, "")
	t.Logf("uploaded=%d bytes, paginated GetLog=%d bytes, begins with first line=%t, ends with last line=%t", len(expected), got.Len(), strings.HasPrefix(got.String(), "FIRST-LINE\n"), strings.HasSuffix(got.String(), "LAST-LINE\n"))
	require.Equal(t, len(expected), got.Len(), "GetLog starting without a page token must expose all uploaded log chunks")
	require.True(t, strings.HasPrefix(got.String(), "FIRST-LINE\n"))
}
