package groups_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bes "google.golang.org/genproto/googleapis/devtools/build/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Send a completed build using the release's BEP wire fields. This creates a
// real invocation owned by the organization for the sharing-policy checks.
func seedInvocation(t *testing.T, c *harness.Clients, label string) string {
	t.Helper()
	field := func(n protowire.Number, b []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
	}
	str := func(n protowire.Number, s string) []byte { return field(n, []byte(s)) }
	number := func(n protowire.Number, v int64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), uint64(v))
	}
	event := func(id, payload protowire.Number, data []byte) []byte {
		return append(field(1, field(id, nil)), field(payload, data)...)
	}
	id := uuid.NewString()
	started := append(str(1, id), number(2, time.Now().UnixMilli())...)
	started = append(started, str(3, "8.0.0")...)
	started = append(started, str(4, "--color=no")...)
	started = append(started, str(5, "test")...)
	started = append(started, str(11, label)...)
	finished := event(9, 14, append(field(3, append(str(1, "SUCCESS"), number(2, 0)...)), number(2, time.Now().UnixMilli()+1500)...))
	finished = append(finished, number(20, 1)...)
	events := [][]byte{event(3, 5, started), event(14, 16, field(1, append(str(1, "BUILD_USER"), str(2, label)...))), event(2, 3, str(1, label+"\n")), finished}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := c.BES.PublishBuildToolEventStream(ctx)
	require.NoError(t, err)
	sid := &bes.StreamId{BuildId: id, InvocationId: id, Component: bes.StreamId_TOOL}
	for i, b := range events {
		require.NoError(t, stream.Send(&bes.PublishBuildToolEventStreamRequest{OrderedBuildEvent: &bes.OrderedBuildEvent{StreamId: sid, SequenceNumber: int64(i + 1), Event: &bes.BuildEvent{EventTime: timestamppb.Now(), Event: &bes.BuildEvent_BazelEvent{BazelEvent: &anypb.Any{TypeUrl: "type.googleapis.com/build_event_stream.BuildEvent", Value: b}}}}}))
	}
	require.NoError(t, stream.CloseSend())
	for range events {
		_, err := stream.Recv()
		require.NoError(t, err)
	}
	_, err = stream.Recv()
	require.Equal(t, io.EOF, err)
	return id
}

// Download and inline-view routes used by the frontend share the invocation ACL.
func checkDownloads(base, id, attempt, label string, public bool, authenticated *http.Client, webBase string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, route := range []string{"download", "view"} {
		for _, artifact := range []string{"raw_json", "buildlog"} {
			path := "/file/" + route + "?" + url.Values{"invocation_id": {id}, "artifact": {artifact}, "attempt": {attempt}}.Encode()
			for _, surface := range []struct {
				client  *http.Client
				base    string
				allowed bool
			}{{client, base, public}, {authenticated, webBase, true}} {
				res, e := surface.client.Get(surface.base + path)
				if e != nil {
					return e
				}
				b, e := io.ReadAll(io.LimitReader(res.Body, 1<<20))
				res.Body.Close()
				if e != nil {
					return e
				}
				if !surface.allowed {
					if res.StatusCode != http.StatusForbidden {
						return fmt.Errorf("private %s %s status %d: %s", route, artifact, res.StatusCode, b)
					}
					continue
				}
				if res.StatusCode != http.StatusOK || !strings.Contains(string(b), label) {
					return fmt.Errorf("%s %s model mismatch HTTP %d: %s", route, artifact, res.StatusCode, b)
				}
			}
		}
	}
	return nil
}

func checkOwnedExecution(c *harness.Clients, action *repb.Digest, org, worker string, owned, blocked bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	stream, e := c.Execution.Execute(ctx, &repb.ExecuteRequest{ActionDigest: action, SkipCacheLookup: true, DigestFunction: repb.DigestFunction_SHA256})
	var response *repb.ExecuteResponse
	if e == nil {
		for {
			op, err := stream.Recv()
			if err != nil {
				e = err
				break
			}
			if !op.Done {
				continue
			}
			if op.GetError() != nil {
				e = status.FromProto(op.GetError()).Err()
				break
			}
			response = &repb.ExecuteResponse{}
			e = op.GetResponse().UnmarshalTo(response)
			if e == nil {
				e = status.FromProto(response.GetStatus()).Err()
			}
			break
		}
	}
	if blocked {
		if status.Code(e) != codes.Unavailable || !strings.Contains(e.Error(), "there was an issue with your request") {
			return fmt.Errorf("blocked execution authorization: %v", e)
		}
		return nil
	}
	if !owned {
		if status.Code(e) != codes.Unavailable || !strings.Contains(e.Error(), "No registered executors in pool") {
			return fmt.Errorf("disabled owned-pool routing: %v", e)
		}
		return nil
	}
	if e != nil {
		return e
	}
	if response == nil || response.GetResult().GetExitCode() != 0 {
		return fmt.Errorf("owned execution result mismatch: %v", response)
	}
	result := response.GetResult()
	if result.GetExecutionMetadata().GetWorker() != worker {
		return fmt.Errorf("owned execution used wrong worker: %s != %s", result.GetExecutionMetadata().GetWorker(), worker)
	}
	stdout := result.GetStdoutRaw()
	if result.GetStdoutDigest() != nil {
		stdout, e = c.Read(ctx, "", result.GetStdoutDigest())
		if e != nil {
			return e
		}
	}
	if string(stdout) != org {
		return fmt.Errorf("owned execution stdout mismatch: %q", stdout)
	}
	return nil
}
