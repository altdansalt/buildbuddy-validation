# Graceful app replacement terminally fails an in-flight task

Title: Graceful app shutdown unregisters the executor before re-enqueuing its leased task
Components: app / executor
Image version/digest: enterprise-v2.314.0 app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; enterprise-v2.314.0 executor `gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8`. Upgrade source app: enterprise-v2.313.0 `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6`.
Severity: medium
Severity justification: A single app/scheduler replacement can fail an already running action despite retained Redis, SQLite and cache and an unchanged executor. Builds may need to resubmit work. No data exposure or privilege boundary violation was observed; multi-node rolling upgrades were not tested.
Status: confirmed
Repro test target: //tests/crosscutting/upgrade-compat:scheduler_shutdown_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/dc8fec13-cdce-436d-9bef-89b2290e6f34

## Expected behavior

With scheduler/task records retained in test-local Redis, the original operation
should recover (possibly retry execution) after a graceful app replacement and
return the shell action's successful output through `WaitExecution`. This is a
scheduler recovery expectation, not a claim that REAPI promises uninterrupted
streams or exactly-once execution during server downtime.

## Actual behavior

Both v2.313.0 -> v2.314.0 upgrade and v2.314.0 -> v2.314.0 replacement return a
terminal `ExecuteResponse.status` with code 14 (`Unavailable`) and message
`No registered executors in pool "" with os "linux" with arch "amd64".`
The RPC itself succeeds; this is the stored terminal outcome of the original
operation, not a transient client connection failure.

The final minimal target, requiring executor readiness after replacement,
reproduced both cases twice:

- https://app.buildbuddy.io/invocation/dc8fec13-cdce-436d-9bef-89b2290e6f34
  (remote run: https://app.buildbuddy.io/invocation/ec723738-ca0c-4bff-92ef-2ac520ec0da7)
- https://app.buildbuddy.io/invocation/d291343d-00d4-4c32-bdfc-58ca4962e5c3
  (remote run: https://app.buildbuddy.io/invocation/e8eae304-1163-4df6-b48f-44cdd6e5e3e1)

The minimal target also reproduced twice before adding the executor-readiness
check: https://app.buildbuddy.io/invocation/507a728b-ad8c-48a1-a901-eaa1f96f9aba
and https://app.buildbuddy.io/invocation/9c49b111-e61d-4ea8-bcd6-6ec262800740.

The initial two-action investigation also reproduced the running failure twice:
https://app.buildbuddy.io/invocation/6ebf306f-c328-4ae1-82f4-a7e222530a15 and
https://app.buildbuddy.io/invocation/5fd27445-916b-44f3-bee7-15b3b0b30711.
Independent verification is recorded below.

## Logs excerpt

From the old app during the two-action upgrade investigation:

```text
Caught terminated signal; starting graceful shutdown (hard-stopping in 25s)
Scheduler: unregistered node "...-executor"
LeaseTask stream closed with task still claimed (shutting_down=true). Re-enqueueing task.
Error fetching action: rpc error: code = FailedPrecondition desc = db is closed
LeaseTask "scheduler-upgrade/uploads/..." tried to re-enqueue task but failed with err:
  rpc error: code = Unavailable desc = No registered executors in pool "" with os "linux" with arch "amd64".
```

The test's post-replacement read of the original operation reports:

```text
Should be zero, but was 14
code:14 message:"No registered executors in pool \"\" with os \"linux\" with arch \"amd64\"."
```

## Notes

Reproduce on hosted runners only:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=init-dockerd=true test \
  //tests/crosscutting/upgrade-compat:scheduler_shutdown_bug_test \
  --test_tag_filters=bug --nocache_test_results --test_arg=-test.v --test_output=all
```

The test uploads an empty input directory and a shell command, waits for
EXECUTING and a shell-created gate marker, and pulls the replacement image before
downtime. It gracefully stops only the app, reuses its `/data` volume and saved
config, retains its peer hostname alias and published ports, and retains the
original Redis and executor. It releases the gate and resumes `WaitExecution`
using the original operation name; it never resubmits Execute. The final fixture
also requires the executor to become ready after replacement and uses a fresh
client connection before observing the terminal result. Docker inspect metadata
and logs from both app containers and the executor are retained as test outputs.

The source app logs generate the terminal failure during shutdown, before the
replacement app starts, ruling out a migration/config error in the new process
as the source of this terminal result. Earlier missing-config, paused-executor,
static-IP setup errors and Docker Hub rate limiting were excluded from evidence.
No shared deployments or dispatch credentials are used by the fixture.

A live executor with one custom resource slot is used for the companion queued
coverage; the minimal running repro has only one action. Supported multi-node
rollout behavior and whether clients are expected to resubmit this terminal
failure require maintainer review. Draining running work before replacement is
a possible operational mitigation.

Source reference:
https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/scheduling/scheduler_server/scheduler_server.go

## Verification

Verdict: **confirmed** (independent review, 2026-10-13). Severity remains
**medium**: replacement of one scheduler loses an accepted, running action and
requires client resubmission, but this does not demonstrate data loss, an
isolation failure, or failure of a multi-app rolling deployment.

Reviewed PR #148 at `7d7c16d`, including `scheduler_bug_test.go`,
`transition_test.go`, the bug target's BUILD rule, and the fixture configuration.
The test waits for both EXECUTING and a marker created by the actual command;
it replaces only the app after pulling the new image, retains Redis and the
app data volume/configuration, and preserves peer aliases and published ports.
It checks replacement-app readiness and executor readiness, then uses a fresh
connection to WaitExecution for the original operation. No second Execute
request or action-cache hit explains the result. The gated command has a
bounded two-minute action timeout; the observed failure is code 14 with the
specific empty-pool message, not an action timeout, container-start failure,
or failed readiness probe. The executor uses supported bare isolation and
fixture-local credentials/endpoints.

The source at the pinned release supplies a concrete recovery invariant and
explains why this is a defect rather than an unsupported upgrade assumption:

- [Scheduler v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/scheduling/scheduler_server/scheduler_server.go#L3084)
  explicitly says that tasks should survive scheduler rollouts by reconnecting,
  even when execution retries are disabled. `LeaseTask` preserves the reconnect
  token during scheduler shutdown (lines 2354–2376), and deliberately closes
  the lease so the client can reconnect (lines 2538–2542).
- In that same file, scheduler shutdown cancels executor registration streams
  (line 422); their deferred cleanup calls `RemoveConnectedExecutor`
  (lines 374–384), which removes the executor's Redis registration
  (lines 1789–1807). `reEnqueueTask` attempts scheduling even for a reconnect
  of a retryable task, and calls `MarkExecutionFailed` if the now-empty pool
  returns Unavailable (lines 3141–3172). Delaying a reservation for the
  reconnect grace period does not delay this empty-pool check. Thus transient
  registration loss during shutdown becomes a permanent operation result.
- [Executor task leaser](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/enterprise/server/scheduling/task_leaser/task_leaser.go#L24)
  enables lease reconnection by default. [Retry policy](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/server/util/platform/platform.go#L916)
  makes this command retryable without additional platform configuration.
- [Released RBE setup documentation](https://github.com/buildbuddy-io/buildbuddy/blob/974929724a4c75f824ab18fbcb560e48bbf2803a/docs/enterprise-rbe.md)
  describes a one-app RBE deployment and Helm upgrades. It does not impose a
  drain-before-upgrade requirement. It also does not promise uninterrupted
  streams, so the verdict relies on the explicit source recovery behavior.
- [REAPI Execution and WaitExecution](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L45)
  allow execution failures in ExecuteResponse.status and client retries;
  WaitExecution may return NOT_FOUND after transient loss. REAPI does not
  require exactly-once execution or survival across downtime. This finding
  is a BuildBuddy shutdown/recovery defect, **not** a strict REAPI violation.

Independent runs used the PR checkout and the exact digest-pinned images listed
above, without image overrides:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test \
  //tests/crosscutting/upgrade-compat:scheduler_shutdown_bug_test \
  --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Run 1](https://app.buildbuddy.io/invocation/b31a290c-aa67-423e-aeb1-992a6cdc3823)
  ([remote runner](https://app.buildbuddy.io/invocation/e8dad5e2-2f4b-426c-a376-561989a7d05c)):
  upgrade and current-version restart both reached the final status assertion
  and returned code 14, `No registered executors in pool "" with os "linux"
  with arch "amd64".` Test durations were 51.64s and 51.73s.
- [Run 2](https://app.buildbuddy.io/invocation/9fbf8edf-e439-444c-92c3-3cf87bd4ca17)
  ([remote runner](https://app.buildbuddy.io/invocation/e684d2f1-7b76-4500-885e-39258c7c8560)):
  both cases reached the same assertion with the identical code and message,
  in 49.96s and 48.95s.

All four independent case executions reproduced the reported terminal result.
The same-version restart rules out upgrade/schema incompatibility as a necessary
cause; successful readiness checks rule out a permanently absent executor.
These observations and the pinned source reject the mundane explanations
above. Multi-app rolling upgrades and other isolation providers remain outside
this finding's demonstrated scope. No test or shared harness change is needed.
