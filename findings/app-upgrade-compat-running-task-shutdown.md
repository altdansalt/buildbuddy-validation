# Graceful app replacement terminally fails an in-flight task

Title: Graceful app shutdown unregisters the executor before re-enqueuing its leased task
Components: app / executor
Image version/digest: enterprise-v2.314.0 app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; enterprise-v2.314.0 executor `gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8`. Upgrade source app: enterprise-v2.313.0 `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6`.
Severity: medium
Severity justification: A single app/scheduler replacement can fail an already running action despite retained Redis, SQLite and cache and an unchanged executor. Builds may need to resubmit work. No data exposure or privilege boundary violation was observed; multi-node rolling upgrades were not tested.
Status: unverified
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
Status remains **unverified** pending maintainer assessment, as requested.

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
