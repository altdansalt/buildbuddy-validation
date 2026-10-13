# Queued execution streams stall after Redis loses scheduler state

Title: Queued Execute and WaitExecution do not report retriable failure after Redis restart
Components: app / executor
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8
Severity: medium
Severity justification: A brief loss of the local Redis scheduling state strands already-admitted queued actions and blocks build clients until their own deadlines, even after new actions execute normally. Requires Redis state loss with availability monitoring disabled (the released default); no security boundary crossing or false successful results were observed.
Status: confirmed
Repro test target: //tests/crosscutting/redis-dependency:queued_restart_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/abb1be93-cebf-49d7-89b5-bb25d36213b3

## Expected behavior

After Redis recovers, each admitted action should either complete with its actual
execution result or receive a clear retriable failure. A lost scheduler task
should not leave clients waiting for an execution that can no longer run.
[REAPI Execute / WaitExecution](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
allows WaitExecution to report NOT_FOUND when an operation no longer exists due
to a transient condition, instructing the client to retry Execute. Errors while
running an action belong in ExecuteResponse.status, not Operation.error.
The suite additionally checks bounded operational recovery; REAPI does not
prescribe a particular recovery time bound.

## Actual behavior

A five-second action occupies the executor's sole execution slot. A second,
zero-delay action is admitted and queued; its command marker does not exist.
The test opens WaitExecution for that second operation and restarts only the
fixture's Redis sidecar. Redis responds PONG after restart. The executor later
attempts to lease the queued task and the app logs `task does not exist`.
Neither the queued Execute stream nor WaitExecution returns completion or a
retriable status before the client's 45-second deadline. A fresh action then
successfully executes and its CAS-backed output is fetched.

The deterministic two-action reduction of seed 314 reproduced twice:

1. [Run 1](https://app.buildbuddy.io/invocation/abb1be93-cebf-49d7-89b5-bb25d36213b3): both queued streams expired at the client deadline; fresh action succeeded.
2. [Run 2](https://app.buildbuddy.io/invocation/5c9455f5-5545-4f28-b8f2-0ed58c5ab305): same outcome on a separate fixture and runner invocation.

These are observed 45-second stalls, not an experimental proof of an infinite
hang. Released source is consistent with an indefinite wait: unmonitored
subscriptions perform `XREAD` with `Block: 0` and do not check whether the
underlying stream or scheduler task disappeared.

Harness/configuration controls:

- [No-Redis-fault control](https://app.buildbuddy.io/invocation/0e3c1378-687e-496a-9610-0a8019b73c0b) with the same single-slot executor, in-flight action, queued action, and merging observers passes.
- [Seeded regression coverage](https://app.buildbuddy.io/invocation/02aaaf6c-3356-4d20-8d54-8341bed26e1f) passes 12 rounds: six pause/stop/restart rounds with Redis availability monitoring enabled, and six Redis pauses with the default monitoring setting.
- No component besides Redis is paused, restarted, or killed in the reproducer. The executor's released `executor.max_concurrent_tasks=1` setting creates a normal scheduling queue. Authentication uses the fixture's disposable local API key.
- An earlier executor-pause experiment also stalled without any Redis fault because it blocked reservation admission. It was discarded and is not evidence for this finding.

Independent verification below confirms the bounded stall on the pinned release.

## Logs excerpt

Run 2; operation name abbreviated consistently as `<queued>`:

```text
00:19:17.292 INF LeaseTask attempt failed: task does not exist execution_id=<queued>
00:19:57.263 INF WaitExecution "<queued>": client disconnected before action completed: context deadline exceeded
queued Execute never received completion/retriable failure after Redis recovery: rpc error: code = DeadlineExceeded desc = stream terminated by RST_STREAM with error code: CANCEL
queued WaitExecution never received completion/retriable failure after Redis recovery: rpc error: code = DeadlineExceeded desc = stream terminated by RST_STREAM with error code: CANCEL
fresh action succeeded after Redis recovery
```

Full app/executor logs, Docker inspect metadata and fixture configs are preserved
in the tests' undeclared outputs. Container startup logs identify version
v2.314.0 and source commit `974929724a4c75f824ab18fbcb560e48bbf2803a`.

## Notes

Reproduce:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test \
  //tests/crosscutting/redis-dependency:queued_restart_bug \
  --test_tag_filters=bug --nocache_test_results --test_output=all --test_arg=-test.v
```

Redis is the harness default
`redis:7.4.2-alpine@sha256:02419de7eddf55aa5bcf49efb74e88fa8d931b4d77c07eff8a6b2144472b6952`,
started with `redis-server --save '' --appendonly no`. Its state loss on restart
is intentional. Other app/executor configs are fixture defaults except the
executor concurrency limit and a test-only bind mount for command markers.
The app's `remote_execution.enable_redis_availability_monitoring` is left at
its released default, false. All images use the pinned defaults; no overrides.

Mitigation supported by the passing coverage: enable
`remote_execution.enable_redis_availability_monitoring=true`. The fixture remains
usable after state loss; failure to notify older queued clients is the defect.
Redis persistence/replication, longer outages, and multi-app failover were not
tested.

Relevant v2.314.0 source:

- [execution_server.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go): monitoring flag defaults false; `pubSubChannelForExecutionID` chooses an unmonitored stream; `waitExecution` converts subscription errors into NOT_FOUND responses.
- [pubsub.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pubsub/pubsub.go): unmonitored stream reads block indefinitely; monitored subscriptions explicitly check for lost stream state.
- [priority_task_scheduler.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/scheduling/priority_task_scheduler/priority_task_scheduler.go): missing queued tasks are dropped after Redis state loss.

Findings stay in this repository; no upstream issue or report was sent.

## Verification

Verdict: **confirmed** (independent review, 2026-10-13). Severity remains
**medium**: a Redis state-loss event strands admitted work under the released
default, but fresh work recovers, no false success is returned, and enabling
availability monitoring mitigates the fault. This is a recovery/liveness defect,
not a claim of durable scheduling across Redis loss or a mandated 45-second REAPI
recovery bound.

Reviewed PR #116 commit `4960f02734bfc26adb2f9e1aea0155e4c1dbb9f9`, including
`restart_bug_test.go`, the Docker discovery helper, action/result helpers, and
property controls. The fixture owns the restarted Redis container; its disabled
RDB/AOF persistence intentionally injects state loss. App/executor containers stay
running. The running marker proves the executor works before the fault; a second
operation is admitted with its marker absent under the supported single-task
concurrency limit. Both independent runs subsequently log `LeaseTask attempt
failed: task does not exist` for that same queued operation. PONG and a fresh
successful action with verified CAS output distinguish lost old work from a
continuing outage, broken auth, executor registration failure, or wrong target.

The test's WaitExecution call is not synchronized on receiving its initial
snapshot before the restart. This limits claims about precisely when its server
subscription was established, but does not explain the already-admitted Execute
stream stalling or make waiting forever on that lost operation correct. The
five-second running action creates a timing window rather than a scheduler
barrier; the marker checks and matching missing-task logs validate that window
in both reviewed runs. The observed failure is deadline expiry, not proof of an
infinite hang. A server-returned execution timeout would need to be distinguished
from client expiry; here the queued task disappears about five seconds in, never
runs, and both observers stay open until their shared client deadline.

[REAPI Execution](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L65)
describes eventual ExecuteResponse or an error; WaitExecution permits NOT_FOUND
for transiently lost operations so clients can retry Execute. It does not require
durable queue storage or specify how quickly loss must be detected. Accordingly,
this verdict concerns failure to notify clients of a discarded task after service
recovery, rather than treating the test's deadline as normative protocol text.

At pinned source commit `974929724a4c75f824ab18fbcb560e48bbf2803a`, the
[monitoring flag](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L93)
explicitly defaults false and describes asking Bazel to retry after Redis state
loss. [Channel selection](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L276)
therefore uses unmonitored streams. [Pubsub](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pubsub/pubsub.go#L260)
checks existence only for monitored streams and otherwise uses blocking XREAD;
subscription errors would produce a terminal NOT_FOUND ExecuteResponse in
[waitExecution](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go#L1379).
The [scheduler claim](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/scheduling/scheduler_server/scheduler_server.go#L2107)
returns NotFound for vanished tasks, and the
[executor scheduler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/scheduling/priority_task_scheduler/priority_task_scheduler.go#L998)
drops that reservation without completing the lost client's stream.

The flag demonstrates a known, optional recovery mechanism; it does not make a
lost task's silent wait a successful recovery. Reviewed released
[RBE setup](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-rbe.md#L36)
and [Redis configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-config.md#L34).
They require/configure Redis but do not document monitoring as mandatory or
exclude state-loss notification from supported behavior. Persistence would avoid
this particular injected loss; it does not explain why the recovered service
continues waiting for a task it has discarded. The scope remains a nonpersistent
Redis restart with monitoring disabled; persistence, replication, and failover
are not verified.

Independent uncached runs on the exact app/executor image digests above:

- [Run 1](https://app.buildbuddy.io/invocation/f7d60823-7f59-41fd-bfc5-9e6b22fb1944): queued Execute and WaitExecution both failed at client DeadlineExceeded; missing-task log and fresh-action success; test duration 75.48s including fixture startup/cleanup.
- [Run 2](https://app.buildbuddy.io/invocation/a78e3148-fc22-46d7-babe-5c8954a27309): same two client-deadline failures, missing-task log, and fresh-action success; test duration 77.09s including startup/cleanup.

Both ran `GIT_CONFIG_GLOBAL=/dev/null bb remote test
//tests/crosscutting/redis-dependency:queued_restart_bug --config=nobug
--test_tag_filters= --nocache_test_results --test_output=all --test_arg=-test.v`
from the PR checkout, without image or test changes. Retrieved complete failed
test logs with `bb view` because the streamed runner output omitted some lines.

Independent controls used the PR's unchanged `go_default_test`, the same remote
flags, and seed 314:

- [No-fault control](https://app.buildbuddy.io/invocation/21bc9127-e112-465e-9802-bb7fd5e6f305), with `--test_arg=-redis.fault=none --test_arg=-redis.iterations=1`: both monitoring-enabled and default-monitoring tests passed, including queued work and fresh-action recovery.
- [Monitoring-enabled restart](https://app.buildbuddy.io/invocation/049150ed-2d94-445b-bb35-01a384f9bf03), with `--test_arg=-test.run=TestRedisFaultProperties --test_arg=-redis.fault=restart --test_arg=-redis.iterations=1`: passed, supporting the documented mitigation.
