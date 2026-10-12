# Executor scheduling regression coverage

Run on hosted runners with fixture-local released containers:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/scheduling/... --config=nobug
```

The suite covers:

- Sixteen distinct simultaneous actions, executor-local queueing with a two-task
  concurrency limit, exact per-action stdout and exit codes, execution timestamp
  interval overlap, and continued service after nonzero command exit codes.
- Six duplicate requests while a cacheable action is in flight, default action
  merging, disconnecting the original stream, and attaching with WaitExecution
  both before and after completion.
- A running executor going away during a task, a second executor on the same
  fixture-local app/network, automatic retry with the original operation name,
  and continued service from the surviving executor.

Contracts come from the [REAPI Execution service](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto),
and released BuildBuddy v2.314.0 source:
[concurrency and queueing](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/scheduling/priority_task_scheduler/priority_task_scheduler.go),
[action merging](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/action_merger/action_merger.go),
[retry decisions](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go),
and [retry platform property](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/platform/platform.go).
REAPI allows redundant execution; the merging assertion checks BuildBuddy's
configured default, not a protocol requirement. The shell tasks are idempotent,
use the bare runner, and use the default retry-enabled platform behavior.

The shutdown test works around the fixture's lack of a Docker control API by
locating its daemon using its exact executor container name. It inspects only
that fixture-owned name and stops only that container. A second fixture uses the
same daemon and first fixture's private network. Cleanup is registered by both
fixtures. No external app/executor is addressed.

Strict multi-organization round-robin fairness, priority ordering, abrupt SIGKILL
lease expiry, retry exhaustion, and hedged executions remain follow-ups. The
finite batch checks eventual progress, not starvation under an infinite load.
