package http_endpoints

import (
	"github.com/altdansalt/buildbuddy-validation/harness"
	"io"
	"testing"
)

func TestMissingInvocationDownload(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	resp, err := f.Web.Client.Get(f.Web.URL + "/file/download?artifact=build_log&invocation_id=00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("nonexistent invocation: HTTP %d; want 404; body=%s", resp.StatusCode, b)
	}
}
