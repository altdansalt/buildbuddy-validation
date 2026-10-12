# Executor timeouts and cancellation

Tests run only shell actions in the digest-pinned released executor container,
with the harness's local app/Redis and bare runner. No host-side Docker exec or
shared deployment is used. Timeout duration checks use executor metadata, so
fixture startup and scheduler queue time do not enter the assertion.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:graceful_timeout_child_test --test_tag_filters=bug --nocache_test_results
```

The regression target covers explicit/default timeouts, maximum-timeout
rejection, partial stdout, normal nonzero exit status, self-signaled actions,
graceful TERM handlers that exit zero, forced termination when TERM is ignored,
ordinary descendant cleanup, Execute context cancellation, connection closure,
and retrieval with WaitExecution after either disconnection. It also checks
preserved app/executor logs for Go panics and fatal runtime errors.

Canceling an Execute subscription is distinct from canceling the underlying
operation. The REAPI makes Operations.CancelOperation optional. These tests
exercise subscription cancellation and disconnect recovery; invocation-level
cancellation through BuildBuddy's UI API remains a follow-up.

The bug target isolates a graceful-timeout parent that exits zero on TERM while
its child ignores TERM and later writes a marker. Both the marker and the probe
are inside the same test-local executor. Child stdio is redirected so inherited
pipes do not keep the parent command's output collection open. The subsequent
probe waits beyond the child's planned write, then reads/removes any marker.
See `findings/executor-timeouts-cancel-graceful-orphan.md`.

Contract references:

- [REAPI Execute and Action.timeout](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
- [Released executor timeout and error handling](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go)
- [Released Unix process tree cleanup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil_unix.go)
- [Released termination-grace-period property](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go)

Limits: bare-runner Linux only; Docker/Firecracker isolation, executor restart,
invocation-level cancellation, and signal-specific OOM classification are not
covered. A self-signaled process must report a failed/killed action, but the
regression does not prescribe a numeric Unix shell signal exit-code convention.
