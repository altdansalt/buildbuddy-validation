# Graceful action timeout leaves a TERM-ignoring descendant running

Title: Graceful action timeout leaves a TERM-ignoring descendant running after the grace period
Components: executor
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: With bare-runner isolation and a nonzero termination grace period, a timed-out action can leave descendants consuming resources or modifying files after its result is finalized. This bypasses the configured execution bound and can interfere with subsequent actions on the same executor. The repro demonstrates a delayed file write inside the executor container; it does not establish a host or cross-organization escape.
Status: unverified
Repro test target: //tests/executor/timeouts-cancel:graceful_timeout_child_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/0a1d5f58-db88-4faf-9780-0472840e3f0a

## Expected behavior

[REAPI Action.timeout](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) specifies a timeout after which execution should be killed. BuildBuddy additionally supports the [`termination-grace-period` platform property](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go): the executor sends graceful termination at the action timeout and sets a force-shutdown deadline at timeout plus grace period. A descendant that ignores TERM should be stopped by that final deadline, including when its parent handles TERM and exits zero.

The released [`commandutil` process-tree cleanup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil_unix.go) explicitly aims to kill descendant processes. The action should not continue making observable changes after timeout and grace have both elapsed.

## Actual behavior

A one-second action with a two-second termination grace period starts a background child that ignores TERM, sleeps five seconds, and writes `survived` to a unique `/tmp` marker. The parent handles TERM by printing `terminated` and exiting zero. Child stdin/stdout/stderr are redirected away from the action's pipes.

The parent returns `ExecuteResponse.status=DEADLINE_EXCEEDED` and exit code `-2`, with stdout `startedterminated`. A subsequent action on the same executor waits six seconds and finds the child's marker. The child therefore ran beyond both the one-second timeout and the three-second force-shutdown deadline.

Independent fixture reproductions both fail on the intended assertion:

1. [First reproduction](https://app.buildbuddy.io/invocation/60caa0e4-bb4a-49c3-98fd-7cf66c171b3b): probe status OK, exit 9, stdout `survived`.
2. [Second reproduction](https://app.buildbuddy.io/invocation/0a1d5f58-db88-4faf-9780-0472840e3f0a): identical probe result; executor logs show graceful termination and DeadlineExceeded at the parent completion.

Harness/configuration checks: each test starts exactly one local app and one released executor, with a fixture-local Redis sidecar. Actions explicitly select `workload-isolation-type=none`, Linux/amd64. The marker includes the unique fixture ID and is only written by the timed-out child; a different action reads/removes it. No Docker exec, shared service, host process, prior fixture, or external API key is involved. Standard hard timeout descendant cleanup and TERM-ignoring top-level force-kill are covered by the passing regression target. Both image references are printed by the fixture; configs, container logs, and Docker inspect metadata are preserved in undeclared test outputs. No Go panic or fatal runtime error was found by the log checks in the second reproduction.

## Logs excerpt

```text
operation ...: status=code:4 message:"deadline exceeded" exit=-2
operation ...: status=<nil> exit=9
Error: Should be zero, but was 9
Messages: descendant survived past timeout and grace period: survived

2026/10/12 23:45:31.930 INF Sending graceful termination signal
2026/10/12 23:45:31.931 WRN Command execution returned error: rpc error: code = DeadlineExceeded desc = deadline exceeded
```

The container log excerpt is from the second reproduction; ANSI formatting and execution/fixture IDs are omitted. Full logs and inspect metadata remain in the test artifacts.

## Notes

Reproduce with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/timeouts-cancel:graceful_timeout_child_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

No image overrides, executor flags, or Docker privileges are used in the repro. The action alone sets `timeout=1s`, `termination-grace-period=2s`, and `DoNotCache=true`; Execute sets `SkipCacheLookup=true`.

Likely source explanation, not a patched-source experiment: [`process.wait`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil_unix.go) closes `p.terminated` when the parent exits. The cleanup goroutine in [`startNewProcess`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil.go) then exits without killing descendants. [`ExecuteTaskAndStreamResults`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go) finalizes the result immediately after graceful termination returns the command result. Its later context cancellation cannot revive the process cleanup goroutine, and the child is now reparented.

A zero grace period avoids the demonstrated path and is covered by the regression test. This report is limited to Linux bare-runner isolation; Docker and Firecracker lifecycle behavior was not tested. Status remains `unverified` for maintainer review, as required by this area assignment. No upstream issue or report was filed.
