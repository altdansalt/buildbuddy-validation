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

Round 2 adds seeded (20261013) shuffled submission of unique priorities behind a
shell-controlled single-slot gate. Executor debug logs prove every reservation
has entered the local queue before releasing the gate. Execution timestamps
check ascending priority within each group and a maximum one-task difference
between every prefix of three equally backlogged organizations. This checks
strict round-robin fairness for equal-sized queued tasks, not an infinite-load
starvation guarantee or preemption. The organizations have separate disposable
API keys; all requests address the fixture-local app.

An abrupt-kill test waits for a command-created marker on each fresh executor,
starts a replacement through `AddExecutor`, proves it can execute with the
current executor's only slot occupied, and then sends SIGKILL. The pool never
loses all registered executors. The five-claim retry budget
must end with INTERNAL and the scheduler's exhaustion message on the original
operation. A fresh action must still succeed afterwards. Claims count towards the budget before shell startup, so the test observes both
fresh command markers and terminal results; it requires at least two injected
kills and rejects a sixth command start. Test-local app lease and grace periods
are shortened to two seconds for bounded recovery.

Hedging is supported in this release. With hedge count one and delay zero, six
additional identical Execute requests must attach to the original operation,
while a shared append-only marker records exactly two command starts. Both
commands are held at a shell gate until the hedge starts. The scripts are
idempotent apart from their fixture-local instrumentation. Hedge behavior is a
BuildBuddy configuration contract, not a REAPI exactly-once requirement.

Lifecycle tests use `Kill`, `Stop`, and `AddExecutor` from PR #63. Until that PR
and the first scheduling PR merge, this branch includes those dependencies.
All builds and component tests run through `bb remote`.
