package caching_results_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func execute(t *testing.T, ctx context.Context, c *harness.Clients, instance string, d *repb.Digest, skip bool, policies ...*repb.ResultsCachePolicy) *repb.ExecuteResponse {
	t.Helper()
	request := &repb.ExecuteRequest{InstanceName: instance, ActionDigest: d, SkipCacheLookup: skip, DigestFunction: repb.DigestFunction_SHA256}
	if len(policies) > 0 {
		request.ResultsCachePolicy = policies[0]
	}
	s, err := c.Execution.Execute(ctx, request)
	require.NoError(t, err)
	for {
		op, err := s.Recv()
		require.NotEqual(t, io.EOF, err, "execution stream ended before completion")
		require.NoError(t, err)
		if !op.Done {
			continue
		}
		require.Nil(t, op.GetError())
		r := &repb.ExecuteResponse{}
		require.NoError(t, op.GetResponse().UnmarshalTo(r))
		require.NotNil(t, r.Result)
		return r
	}
}

func action(t *testing.T, ctx context.Context, c *harness.Clients, instance, script string, noCache bool, timeout time.Duration) *repb.Digest {
	t.Helper()
	root, err := c.UploadProto(ctx, instance, &repb.Directory{})
	require.NoError(t, err)
	command, err := c.UploadProto(ctx, instance, &repb.Command{
		Arguments:   []string{"/bin/sh", "-c", script},
		OutputPaths: []string{"nonce.txt"},
		Platform:    &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}}},
	})
	require.NoError(t, err)
	d, err := c.UploadProto(ctx, instance, &repb.Action{CommandDigest: command, InputRootDigest: root, DoNotCache: noCache, Timeout: durationpb.New(timeout)})
	require.NoError(t, err)
	return d
}

func ac(t *testing.T, ctx context.Context, c *harness.Clients, instance string, d *repb.Digest) (*repb.ActionResult, error) {
	t.Helper()
	return c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256})
}

// A fresh random output distinguishes actual execution from a mislabeled cache hit.
const nonce = "cat /proc/sys/kernel/random/uuid > nonce.txt; "

func TestCacheLifecycle(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for _, tc := range []struct {
		name, script string
		noCache      bool
		exit         int32
		status       codes.Code
	}{
		{"success", nonce + "exit 0", false, 0, codes.OK},
		{"do_not_cache", nonce + "exit 0", true, 0, codes.OK},
		{"failed", nonce + "exit 17", false, 17, codes.OK},
		{"deadline", nonce + "sleep 5", false, 0, codes.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := "cache-lifecycle-" + tc.name
			timeout := 20 * time.Second
			if tc.status != codes.OK {
				timeout = time.Second
			}
			d := action(t, ctx, c, instance, tc.script, tc.noCache, timeout)
			_, err := ac(t, ctx, c, instance, d)
			require.Equal(t, codes.NotFound, status.Code(err))
			var previous string
			for i := 0; i < 2; i++ {
				r := execute(t, ctx, c, instance, d, false)
				require.Equal(t, int32(tc.status), r.GetStatus().GetCode(), "%v", r.GetStatus())
				if tc.status == codes.OK {
					require.Equal(t, tc.exit, r.Result.ExitCode)
				}
				cacheable := !tc.noCache && tc.exit == 0 && tc.status == codes.OK
				require.Equal(t, cacheable && i == 1, r.CachedResult)
				cached, err := ac(t, ctx, c, instance, d)
				if cacheable {
					require.NoError(t, err)
					require.True(t, proto.Equal(r.Result.OutputFiles[0].Digest, cached.OutputFiles[0].Digest), "digest values differ")
				} else {
					require.Equal(t, codes.NotFound, status.Code(err))
				}
				require.Len(t, r.Result.OutputFiles, 1)
				b, err := c.Read(ctx, instance, r.Result.OutputFiles[0].Digest)
				require.NoError(t, err)
				if i == 1 {
					if cacheable {
						require.Equal(t, previous, string(b))
					} else {
						require.NotEqual(t, previous, string(b))
					}
				}
				previous = string(b)
			}
		})
	}
}

func TestSkipLookupReplacesCachedResult(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	instance := "skip-cache-lookup"
	d := action(t, ctx, c, instance, nonce+"exit 0", false, 20*time.Second)
	// Seed a valid but recognizably different entry without executing the action.
	seed, err := c.Upload(ctx, instance, []byte("seeded result\n"))
	require.NoError(t, err)
	_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256, ActionResult: &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "nonce.txt", Digest: seed}}}})
	require.NoError(t, err)
	hit := execute(t, ctx, c, instance, d, false)
	require.True(t, hit.CachedResult)
	require.True(t, proto.Equal(seed, hit.Result.OutputFiles[0].Digest), "digest values differ")
	fresh := execute(t, ctx, c, instance, d, true)
	require.Zero(t, fresh.GetStatus().GetCode())
	require.Zero(t, fresh.Result.ExitCode)
	require.False(t, fresh.CachedResult)
	require.Len(t, fresh.Result.OutputFiles, 1)
	require.False(t, proto.Equal(seed, fresh.Result.OutputFiles[0].Digest), "execution must produce a different digest")
	cached, err := ac(t, ctx, c, instance, d)
	require.NoError(t, err)
	require.True(t, proto.Equal(fresh.Result.OutputFiles[0].Digest, cached.OutputFiles[0].Digest), "digest values differ")
	again := execute(t, ctx, c, instance, d, false)
	require.True(t, again.CachedResult)
	require.True(t, proto.Equal(fresh.Result.OutputFiles[0].Digest, again.Result.OutputFiles[0].Digest), "digest values differ")
	// Identical action bytes in another instance must not hit the original AC entry.
	other := action(t, ctx, c, instance+"-other", nonce+"exit 0", false, 20*time.Second)
	require.True(t, proto.Equal(d, other), "digest values differ")
	isolated := execute(t, ctx, c, instance+"-other", other, false)
	require.False(t, isolated.CachedResult)
	require.Zero(t, isolated.GetStatus().GetCode())
}

// An unsuccessful or non-cacheable forced execution must not replace a valid AC entry.
func TestSkipLookupDoesNotWriteIneligibleResults(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, tc := range []struct {
		name    string
		noCache bool
		exit    int32
	}{
		{"failed", false, 23}, {"do_not_cache", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := "preserve-seed-" + tc.name
			script := nonce + "exit 23"
			if tc.exit == 0 {
				script = nonce + "exit 0"
			}
			d := action(t, ctx, c, instance, script, tc.noCache, 20*time.Second)
			seed, err := c.Upload(ctx, instance, []byte("known previous result\n"))
			require.NoError(t, err)
			_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{
				InstanceName: instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256,
				ActionResult: &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "nonce.txt", Digest: seed}}},
			})
			require.NoError(t, err)
			r := execute(t, ctx, c, instance, d, true)
			require.Zero(t, r.GetStatus().GetCode())
			require.Equal(t, tc.exit, r.Result.ExitCode)
			require.False(t, r.CachedResult)
			require.Len(t, r.Result.OutputFiles, 1)
			require.False(t, proto.Equal(seed, r.Result.OutputFiles[0].Digest), "execution must produce a different digest")
			cached, err := ac(t, ctx, c, instance, d)
			require.NoError(t, err)
			require.True(t, proto.Equal(seed, cached.OutputFiles[0].Digest), "digest values differ")
		})
	}
}

// Cache lookup must faithfully return a manually cached nonzero exit code.
// A forced execution must replace that entry with the successful real result.
func TestCachedFailureResponse(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	instance := "cached-failure"
	d := action(t, ctx, c, instance, nonce+"exit 0", false, 20*time.Second)
	seed, err := c.Upload(ctx, instance, []byte("cached failure output\n"))
	require.NoError(t, err)
	want := &repb.ActionResult{ExitCode: 17, OutputFiles: []*repb.OutputFile{{Path: "nonce.txt", Digest: seed}}}
	_, err = c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256, ActionResult: want})
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		hit := execute(t, ctx, c, instance, d, false)
		require.Zero(t, hit.GetStatus().GetCode())
		require.True(t, hit.CachedResult)
		require.Equal(t, want.ExitCode, hit.Result.ExitCode)
		require.Len(t, hit.Result.OutputFiles, 1)
		require.True(t, proto.Equal(want.OutputFiles[0], hit.Result.OutputFiles[0]))
	}
	fresh := execute(t, ctx, c, instance, d, true)
	require.Zero(t, fresh.GetStatus().GetCode())
	require.Zero(t, fresh.Result.ExitCode)
	require.False(t, fresh.CachedResult)
	require.Len(t, fresh.Result.OutputFiles, 1)
	require.False(t, proto.Equal(seed, fresh.Result.OutputFiles[0].Digest))
	hit := execute(t, ctx, c, instance, d, false)
	require.True(t, hit.CachedResult)
	require.Zero(t, hit.Result.ExitCode)
	require.True(t, proto.Equal(fresh.Result.OutputFiles[0].Digest, hit.Result.OutputFiles[0].Digest))
}

// ResultsCachePolicy affects server-defined retention, not action identity or
// eligibility to write the cache. No eviction ordering is assumed here.
func TestResultsCachePolicyPriority(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	c := f.Clients(harness.App)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for _, priority := range []int32{-1, 0, 1} {
		for _, noCache := range []bool{false, true} {
			t.Run(fmt.Sprintf("priority_%d/do_not_cache_%t", priority, noCache), func(t *testing.T) {
				instance := fmt.Sprintf("priority-%d-%t", priority, noCache)
				d := action(t, ctx, c, instance, nonce+"exit 0", noCache, 20*time.Second)
				policy := &repb.ResultsCachePolicy{Priority: priority}
				first := execute(t, ctx, c, instance, d, false, policy)
				require.Zero(t, first.GetStatus().GetCode())
				require.Zero(t, first.Result.ExitCode)
				require.False(t, first.CachedResult)
				require.Len(t, first.Result.OutputFiles, 1)
				cached, err := ac(t, ctx, c, instance, d)
				if noCache {
					require.Equal(t, codes.NotFound, status.Code(err))
				} else {
					require.NoError(t, err)
					require.Len(t, cached.OutputFiles, 1)
					require.True(t, proto.Equal(first.Result.OutputFiles[0].Digest, cached.OutputFiles[0].Digest))
				}
				// Changing only the requested retention priority must not hide a cache hit.
				second := execute(t, ctx, c, instance, d, false, &repb.ResultsCachePolicy{Priority: -priority})
				require.Zero(t, second.GetStatus().GetCode())
				require.Zero(t, second.Result.ExitCode)
				require.Equal(t, !noCache, second.CachedResult)
				require.Len(t, second.Result.OutputFiles, 1)
				require.Equal(t, !noCache, proto.Equal(first.Result.OutputFiles[0].Digest, second.Result.OutputFiles[0].Digest))
			})
		}
	}
}
