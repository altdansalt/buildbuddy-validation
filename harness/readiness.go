package harness

import (
	"context"
	"fmt"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
)

// WaitExecutionReady waits for the app and scheduler to complete a disposable
// uncached bare-runner action. Call after restarting an app or executor when the
// test needs scheduling readiness, rather than only component HTTP readiness.
// Requires a local app, a bare-runner executor, and an execution-capable API key.
func (f *Fixture) WaitExecutionReady() {
	c := f.Clients(App)
	defer c.Conn.Close()
	deadline := time.Now().Add(f.opts.StartupTimeout)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		last = executionProbe(ctx, c)
		cancel()
		if last == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	f.t.Fatalf("execution/scheduler readiness timed out: %v", last)
}

func executionProbe(ctx context.Context, c *Clients) error {
	const instance = "harness-readiness"
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	if err != nil {
		return err
	}
	cmd, err := c.UploadProto(ctx, instance, &repb.Command{Arguments: []string{"/bin/true"}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{
		{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"},
	}}})
	if err != nil {
		return err
	}
	action, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: cmd, InputRootDigest: root, DoNotCache: true})
	if err != nil {
		return err
	}
	stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: instance, ActionDigest: action, SkipCacheLookup: true})
	if err != nil {
		return err
	}
	for {
		op, err := stream.Recv()
		if err != nil {
			return err
		}
		if !op.Done {
			continue
		}
		if op.GetError() != nil {
			return fmt.Errorf("probe operation: %v", op.GetError())
		}
		result := &repb.ExecuteResponse{}
		if err := op.GetResponse().UnmarshalTo(result); err != nil {
			return err
		}
		if result.GetStatus().GetCode() != 0 || result.Result == nil || result.Result.ExitCode != 0 {
			return fmt.Errorf("probe execution: %v", result)
		}
		return nil
	}
}
