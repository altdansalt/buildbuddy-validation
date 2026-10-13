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

## Round 2 extension and remaining coverage

The audit's browser `BuildBuddyService.WaitExecution` framed HTTP stream check
is retained with the original expectations. Real actions now exit 7 and emit
stdout, stderr, and a declared output file; file and log bytes are read from app
and proxy. Standard ExecuteResponse fields are compared in full across history,
HTTP, and the cached response (auxiliary metadata enrichment is explicitly
excluded). History status, exit code, command snippet, action digest agreement, and
consistent omission of legacy task-size/result-digest fields are checked. Three completion samples spaced 100 ms apart repeat all
history/cache comparisons; test logs report elapsed completion convergence.
Synthetic TaskExists is checked three times through each of two same-org keys.

Additional source enumeration:
- `app/invocation/execution_status.tsx`: browser streaming WaitExecution (checked).
- `app/invocation/invocation_action_card.tsx`: GetExecutionDownloads, command and
  action CAS fetches, `/file/download?artifact=execution_profile`. Downloads,
  output file HTTP bytes and profile run-command timing are now checked.
- `enterprise/server/execution_service/execution_service.go:458`: downloads
  require a persisted, authorized invocation, not merely request metadata.
- `enterprise/server/execution_search_service/execution_search_service.go`:
  SearchExecution and GetExecutionTimeline require analytics storage (not
  configured by this fixture).
- `cli/execution/execution.go`: `bb execution get` reads the cached response;
  the released CLI process is now checked against both app and proxy.

This extension does **not** establish exhaustive conformance. Remaining work:
analytics fixture; controlled queued snapshots; exact history action/result digest and metadata
oracle; warmed cache updates/deletes in both directions; and precise node metadata
oracles. Node observations also accompany every synthetic TaskExists check. Queue/active counts here validate reported
registration state, not scheduler-derived metrics. Completion stability covers
300 ms and cannot establish indefinite stability. No divergence finding is
claimed from absent coverage or configuration requirements.

The test downloads released CLI 5.0.492 from GitHub and checks SHA256
`e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.
Each CLI process has an isolated HOME and explicit fixture-local target/key; it
inherits no dispatch credentials. A valid two-event BES invocation is persisted
and checked before downloading execution artifacts. One input is written via
the proxy and consumed by the executor through the app. Download lists agree
in gRPC and browser JSON and their entries match the input digest; file-cache
hits legitimately produce no remotely-downloaded entry. Output bytes and
execution profile command-span timestamps/durations match the execution result.

Real actions run for nine seconds so the intermediate EXECUTING history update
can be observed. The released PublishOperation implementation intentionally
delays its database update until over five seconds have elapsed, with a
one-second ticker; the test permits eight seconds and reports observed time.
This expected transient history lag is distinct from a permanent mismatch.
HTTP/CLI comparisons cover every standard REAPI JSON field, while unknown
release extension fields need generated release-specific types for independent
semantic validation. No product defect is claimed from those projection limits.

Source inspection resolves the ACK acceptance question: unary
EnqueueTaskReservation calls enqueueOnConnectedExecutor with
`waitForResponse=true`, and executorHandle returns success only after consuming
the task-specific response channel. Thus the existing successful unary response
and the independently received matching reservation establish server-side ACK
acceptance; merely successful ACK SendMsg would not suffice. See
`scheduler_server.go:516`, `:565`, `:2952`, and `:3061`.

Round 2 remote validation:
- Expanded CLI/browser/download/profile suite passed:
  https://app.buildbuddy.io/invocation/ce65a53b-3906-47e2-8577-ca027e95c955
- Final duration oracle passed without cached test results:
  https://app.buildbuddy.io/invocation/f7f62c75-19e0-4892-a785-b0d809e786cf
- Intermediate-stage probes measured 5.006 s and 5.204 s to history convergence,
  consistent with the release's deliberate delayed DB update. Those development
  runs subsequently failed on corrected HTTP request-context and microsecond
  rounding assumptions, respectively; neither failure is a product finding:
  https://app.buildbuddy.io/invocation/04f8bf39-b85a-47aa-8b53-498d0cf87a36
  https://app.buildbuddy.io/invocation/0893ec40-500d-4a35-80ad-13f4454cf5e9
- Completed views converged in 111–118 ms in these probes; registration checks
  took 303–307 ms including the required three-sample stability interval. These
  measurements are polling observations, not upper bounds on all deployments.

No unexpected divergence was confirmed. No finding, shared harness change, or
PR is included. The remaining coverage above is explicitly unfinished.
