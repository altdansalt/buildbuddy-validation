# Scheduler state conformance (enterprise v2.314.0)

Run `bb remote test //tests/statemachine/scheduler-state:go_default_test --test_output=errors`.
The fixture starts the released app, executor, cache proxy and local Redis. No
component RPC addresses a shared deployment. All keys are created by the local
app; GetExecutionNodes requires a separately provisioned ORG_ADMIN key.

## Protocol and read-surface inventory

Inspected the released source in `~/buildbuddy-src`:

- `proto/scheduler.proto`: the complete Scheduler service (five mutations and
  TaskExists), registration message variants, lease and reservation envelopes.
- `enterprise/server/scheduling/scheduler_server/scheduler_server.go`:
  executorHandle.Serve, ScheduleTask, EnqueueTaskReservation, LeaseTask,
  ReEnqueueTask, GetExecutionNodes, Redis claim/release scripts, and cancellation.
- `enterprise/server/scheduling/task_leaser/task_leaser.go`: normal executor
  lease renewals, stream completion and the unary re-enqueue fallback.
- `proto/remote_execution.proto`: supported ExecutionTask payload fields.
- `server/buildbuddy_server/buildbuddy_server.go`,
  `server/capabilities_filter/capabilities_filter.go`,
  `proto/execution_stats.proto`, and
  `enterprise/server/execution_service/execution_service.go`: registration and
  execution reads exposed to the UI. GetExecution requires an invocation ID.
- `server/util/rexec/rexec.go`: execution responses are cached in an ActionResult
  keyed by the digest of the execution ID. Cached/history responses deliberately
  retain more metadata than Execute streams, so shared outcome fields are the oracle.
- `enterprise/server/cmd/cache_proxy/cache_proxy.go`: the proxy registers cache
  APIs, not Scheduler, Execution or BuildBuddyService. Scheduler state has no
  proxy reader; the cached execution response does have app/proxy readers.
- `cli/cli_command/register/register.go`, `cli/execute/execute.go`, and
  `cli/execution/execution.go`: no scheduler-state CLI reader. The execution
  viewer uses the cached response; that backing record is checked directly here.

| Mutating RPC | Read surfaces and transitions checked |
| --- | --- |
| RegisterAndStreamWork | GetExecutionNodes gRPC + UI HTTP: absent, registered, repeated registration without duplicates, queue/active count updates, graceful shutdown and removal. Reservation ACKs follow the released stream protocol. Idle AskForMoreWork gets bounded backoff. |
| ScheduleTask | TaskExists false → true; reservation stream task ID + pool; supported ExecutionTask returned by lease; repeated scheduling returns AlreadyExists and preserves state; recreate after finalization. |
| EnqueueTaskReservation | Two valid repeated requests for an existing task; each reservation is delivered and ACKed, TaskExists stays true. |
| LeaseTask | Initial payload, positive lease duration and lease ID; repeated renewal omits payload and preserves task; supported legacy release preserves task; stream re-enqueue delivers a new reservation; reacquisition gets a new lease ID and the same payload; finalization closes cleanly, removes task, and emits proactive reservation cancellation. |
| ReEnqueueTask | Valid unary handoff after clean legacy release with the last issued lease ID; TaskExists stays true, reservation delivered, new lease ID and original task payload on reacquisition. |
| Execution.Execute (scheduler integration) | Two actual actions on the released executor: completed Execute agrees with repeated WaitExecution, GetExecution gRPC + UI HTTP, and cached ExecuteResponse from app + proxy ActionCache; TaskExists eventually becomes false. |

A small reference model tracks task existence and executor registration/counts.
Two full scheduler lifecycles use the same task ID to cover valid reuse. Scheduler
reservation delivery is read continuously and ACKed before proceeding. Writes
on the registration stream are serialized. Read convergence uses bounded polling
(30 seconds, 100 ms cadence), and streams/RPCs share a four-minute context.

The synthetic executor uses an isolated pool and never competes with the real
executor. Direct ScheduleTask does not create an Execution operation or history
record: those are created by Execution.Execute. Accordingly, synthetic tasks
are checked through scheduler surfaces; real Execute tasks are checked through
execution/history/cache surfaces. No malformed requests, lease loss, fault
injection, expiration, or unauthorized requests are used.

Wire envelopes carry release-defined protobuf fields without importing or
building BuildBuddy generated/source code. Standard REAPI payloads use generated
remote-apis types. Lease-time experiment additions are allowed; the original
ExecuteRequest, Action, Command, and task ID must remain unchanged.

## Pinned fixture images

- App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- Executor: `gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8`
- Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

All three are enterprise-v2.314.0. The harness preserves actual image IDs,
container logs and configs in test undeclared outputs. No shared harness or
dependency changes are needed.

## Validation

- Full lifecycle and execution/history/cache checks passed:
  https://app.buildbuddy.io/invocation/131ce03b-7b75-4a48-a60b-cca652e49ca8
- Final version, including executor identity/capacity/check-in assertions, passed
  with `--nocache_test_results`:
  https://app.buildbuddy.io/invocation/2d7d6d4a-056b-4809-b849-27290c9580b2

No confirmed cross-surface inconsistency was found. Initial development failures
were due to the default key lacking ORG_ADMIN, ordering unary re-enqueue before
legacy stream release, comparing deliberately different metadata projections,
and using structural equality instead of protobuf equality for digests.
