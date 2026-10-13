package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/emptypb"
)

// This is a read-only baseline, not workflow transition coverage. The empty
// protobuf is wire-compatible with GetWorkflowsRequest with no request context:
// the fixture-local API key selects the group on the gRPC surface.
func TestWorkflowReadBaseline(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	c := f.Clients(harness.App)
	for step := 0; step < 2; step++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		response := &emptypb.Empty{}
		err := c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetWorkflows", &emptypb.Empty{}, response)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		// Decode unknown response fields rather than discarding the workflow list.
		data := response.ProtoReflect().GetUnknown()
		for len(data) > 0 {
			number, typ, n := protowire.ConsumeTag(data)
			if n < 0 {
				t.Fatal("invalid response tag")
			}
			data = data[n:]
			if number == 2 {
				t.Fatal("fresh fixture unexpectedly has a workflow")
			}
			n = protowire.ConsumeFieldValue(number, typ, data)
			if n < 0 {
				t.Fatal("invalid response field")
			}
			data = data[n:]
		}
		ui, err := f.Web.RPC("GetWorkflows", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := ui["workflow"]; ok && value != nil {
			list, ok := value.([]any)
			if !ok || len(list) != 0 {
				t.Fatalf("UI workflow list: %v", value)
			}
		}
	}
}
