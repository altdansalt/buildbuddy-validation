package http_endpoints

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

// JSON protolet promises application/json, but sets that header after Write.
func TestProtoletJSONContentType(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	r, err := http.NewRequest("POST", f.Web.URL+"/rpc/BuildBuddyService/GetUser", bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := f.Web.Client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GetUser status=%d body=%s", resp.StatusCode, b)
	}
	ct, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || ct != "application/json" {
		t.Fatalf("JSON RPC returned Content-Type %q; want application/json; body=%s", resp.Header.Get("Content-Type"), b)
	}
}
