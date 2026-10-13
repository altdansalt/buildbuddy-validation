# Executor lifecycle

Tests run released containers exclusively in fixture-local networks:

- registration health (query and header probes), graceful idle shutdown, restart and fresh execution;
- SIGTERM during a command confirmed running by a container-local marker, readiness withdrawal, liveness during lameduck, successful result delivery and graceful exit;
- adjacent-release app/executor skew in both directions (v2.313.0 against pinned v2.314.0);
- invalid build-root startup failure with a useful diagnostic, followed by recovery with the original configuration.

Behavior references at the pinned release:
[executor initialization](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/executor/executor.go),
[scheduler registration and shutdown](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/scheduling/scheduler_client/scheduler_client.go),
[health checks](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/healthcheck/healthcheck.go), and
[REAPI Execute](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).
Version skew coverage records observed compatibility of this release pair; REAPI does not promise arbitrary internal scheduler protocol compatibility.

The tests use the harness lifecycle API, including expected startup failure and an additional executor. All mutations address fixture-owned containers.

Round 2 fault coverage adds:

- SIGKILL after a command-start marker, with a second executor available to reschedule the original action;
- SIGTERM while one action occupies the executor's single configured CPU slot and three distinct actions are queued, then a terminal result or explicit retriable failure after a replacement executor is added;
- app SIGKILL/restart while an executor holds a gated command, reconnection with `WaitExecution` to the original operation, and a command-attempt counter.

The observer requires one terminal response with matching stdout/exit status, or a diagnostic `Unavailable`, `Aborted`, or `ResourceExhausted` status. A client-side deadline or premature EOF is a failure. Each action is submitted once; reconnecting an observer never calls Execute again. This checks terminal delivery, not exactly-once side effects, which REAPI cannot guarantee when an executor dies mid-command. The app-outage case checks that the surviving executor does not rerun its held command.

Queued submission order is randomized with default seed 6302, logged by the test. Replay with `--test_arg=-lifecycle.seed=6302`. Commands use unique fixture IDs, `DoNotCache`, and `SkipCacheLookup` so results cannot be supplied by an earlier execution. All RPCs have bounded contexts and all lifecycle mutations address fixture-owned containers.

The app-outage test flushes uploaded inputs with a graceful app restart and reads them back before submitting work, then waits for executor scheduling readiness. Without this preflight, killing the app immediately after uploading small CAS blobs twice resulted in a missing action digest and retry-budget exhaustion; the local Pebble cache uses unsynced writes. That experiment did not isolate a lifecycle defect and is not reported as one. The actual outage still uses SIGKILL while the command is running. With durable inputs, the full suite passed in [this invocation](https://app.buildbuddy.io/invocation/c9d8635c-165d-4228-ae3f-2a8b1563f00f).

Final validation of the strengthened response checks passed with seed 6303 and fixture-owned Docker daemons: [BuildBuddy invocation](https://app.buildbuddy.io/invocation/7e5986d8-152c-49ce-887d-2d330c7d7414). SIGKILL work completed on the replacement; all three queued actions returned explicit `Unavailable` statuses while no executor was registered during the drain gap; the held app-outage action completed through `WaitExecution` with one command attempt. These results satisfy the completion-or-retriable-failure invariant. No isolated lifecycle finding was filed.
