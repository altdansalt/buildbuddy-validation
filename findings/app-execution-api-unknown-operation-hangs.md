# WaitExecution waits indefinitely for an unknown operation

Title: Valid unknown WaitExecution operation names wait until the client deadline
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Clients cannot promptly detect lost or invalid operation state and resubmit work. An unbounded client can retain a streaming RPC indefinitely. This requires authenticated access to the execution service; no cross-group exposure was demonstrated.
Status: confirmed
Repro test target: //tests/app/execution-api:unknown_operation_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/1989ec98-ec5f-4983-bd43-c70e5a05d6d2

## Expected behavior

[REAPI WaitExecution](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) specifies NOT_FOUND when the operation no longer exists. A syntactically valid but never-created name on a fresh app should promptly return NOT_FOUND, allowing the client to recover.

## Actual behavior

Two independent fresh fixtures returned no operation and no server error during a five-second bounded wait. The client eventually saw DeadlineExceeded:

- https://app.buildbuddy.io/invocation/1989ec98-ec5f-4983-bd43-c70e5a05d6d2
- https://app.buildbuddy.io/invocation/d5b90b76-45e8-4b43-8a1c-2b413b94333a

The fixture had completed readiness and authenticated local group/key provisioning. The operation name uses the same upload-resource-name syntax as real executions, with a fresh UUID and a SHA256 digest. No Execute request was submitted; an executor is intentionally unnecessary. Both reproductions used the default server configuration. Malformed names return InvalidArgument in existing passing coverage, and the monitored-stream configuration is covered separately as a recovery control. Independent verification below confirms the defect.

## Logs excerpt

```text
WaitExecution("unknown/uploads/c1f64be6-0a2e-4fea-af5e-c6791a0f7ff5/blobs/4cb744a08586a376b416d59f87c853398af16764d4966779b513ec4c9fcbf1bb/14"):
rpc error: code = DeadlineExceeded desc = context deadline exceeded
expected: NotFound
actual: DeadlineExceeded
```

## Notes

Run `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/execution-api:unknown_operation_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all` after configuring the public fetch remote.

Released-source inspection shows the default WaitExecution path subscribes to an unmonitored Redis stream without checking operation existence. The opt-in `--remote_execution.enable_redis_availability_monitoring=true` path checks missing streams and sends a terminal ExecuteResponse with NOT_FOUND. The separate passing regression target also verifies completed replay and expiration using a shortened TTL on only the fixture-owned operation stream. The five-second repro establishes absent prompt rejection, not a measured upper bound on how long the default stream could remain open. No shared deployment or worker dispatch credential is involved.

## Verification

Verdict: **confirmed** (independent review, 2026-10-13). Reviewed PR #89 at
`dc1a618f8b75e2e63dc9e18540c249e34df21d25`, including
`unknown_operation_test.go`, the BUILD target and harness auth/resource-name helpers.
Two uncached hosted-runner executions independently started fresh local app
fixtures and failed only the NotFound assertion: no operation was received before
the five-second RPC deadline, then the client returned DeadlineExceeded. Fixture
startup and self-auth completed before that deadline began. An executor cannot
make a never-submitted operation exist and is unnecessary for this check.

Command for both runs:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/execution-api:unknown_operation_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Run 1: test invocation](https://app.buildbuddy.io/invocation/7aa805eb-eea0-45f4-abdf-0013e563945d), [runner invocation](https://app.buildbuddy.io/invocation/f7e01955-c028-47ed-9ca2-447ee09c2f80).
- [Run 2: test invocation](https://app.buildbuddy.io/invocation/2b0c38fd-e360-47f1-8006-4bb9cee1c376), [runner invocation](https://app.buildbuddy.io/invocation/3ee215c6-ae40-4458-95f8-67f21eef5a0c).

The [REAPI contract at 6def1c5d27a5](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L122-L138)
says the initial response is immediate and specifically lists an unknown operation
name as a NOT_FOUND case. Although the error list says "may fail", it does not
justify waiting for a nonexistent execution to complete. The five-second timeout
is a bounded observation, not empirical proof of an infinite lifetime.

The pinned [execution server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L1324-L1389)
authenticates and parses the upload resource name, then subscribes without an
existence check. Its default availability-monitoring flag is false. The
[pinned pubsub implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pubsub/pubsub.go#L260-L339)
accepts an empty tail read and performs XREAD with `Block: 0` (indefinite blocking);
only monitored channels check existence. This explains the reproducible silence
without relying on container slowness or networking failure. A fresh UUID, valid
SHA256 digest and isolated Redis rule out collisions or stale operation state.

The released [RBE configuration documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-rbe.md)
documents enabling remote execution, as this fixture does, without requiring
availability monitoring. The opt-in recovery path is a workaround, not a reason
to reject default-configuration behavior. Severity remains **medium**: failure to
detect missing operation state obstructs retry/recovery, but this test proves no
crash, cross-group access or ordinary execution data loss. The exact app image
and digest above remain unchanged; no image or config override was used.

Additional disproof control: the existing
`//tests/app/execution-api:go_default_test` target passed uncached on the same
PR #89 checkout using `--config=nobug --test_tag_filters=`. Its malformed-name,
normal execution/replay and monitored unknown/expired-stream checks passed,
including a five-second bound for the monitored missing-stream response. This
supports the default-path diagnosis and rules out a generally broken execution
fixture. [Control test invocation](https://app.buildbuddy.io/invocation/9085b8b7-675c-4b96-950c-51e38e623ccc),
[control runner invocation](https://app.buildbuddy.io/invocation/4c05d348-c6a3-4161-bcdc-b0f5f8c00599).
