# Redis dependency properties

All tests start released app and executor containers and use the fixture's Redis
image. They fault only Redis in that fixture. No extra images or host credentials
are used. `executor.max_concurrent_tasks=1` holds one action in-flight and another
queued across the fault without interrupting admission or lease renewals.

`//tests/crosscutting/redis-dependency:go_default_test` contains:

- `TestRedisFaultProperties`: seeded Redis pauses, stop/start, and restarts with
  `remote_execution.enable_redis_availability_monitoring=true`.
- `TestDefaultRedisPauseProperties`: seeded Redis pauses with the released
  default availability-monitoring setting (false).

Each seed varies action identity, run duration, and outage duration. Two
identical Execute requests exercise pending action merging; an independently
subscribed WaitExecution observes the first operation. Successful results must
have the expected CAS-backed output, terminal streams must end, and other
outcomes must be clearly retriable. A fresh action must succeed after recovery.
Client deadline expiry is a failure. A bind-mounted append-only command marker
checks actual execution count. App and executor logs are checked for runtime
panic signatures; the fixture preserves full container logs and image metadata.

Replay or extend (default six rounds per property, maximum nine):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/redis-dependency/... \
  --config=nobug --nocache_test_results --test_output=all \
  --test_arg=-test.v --test_arg=-redis.seed=314 --test_arg=-redis.iterations=9
```

Diagnostic controls: `-redis.fault=none` runs without a Redis fault;
`-redis.queue=false` omits the queued action. Randomness is deterministic;
concurrent scheduling and network timing are not. Failures log the seed, round,
fault duration and operation name. RPCs have bounded deadlines.

`//tests/crosscutting/redis-dependency:queued_restart_bug` is the fixed reduction
of seed 314: one five-second running action, one queued action, one Redis restart,
and default unmonitored streams. It checks both Execute and WaitExecution for
the queued action, and proves a fresh action succeeds after Redis recovery.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test \
  //tests/crosscutting/redis-dependency:queued_restart_bug \
  --test_tag_filters=bug --nocache_test_results --test_output=all --test_arg=-test.v
```

Source / contract references:

- [v2.314.0 execution server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go):
  Redis availability monitoring defaults to false; `waitExecution` maps
  subscription failures to a completed NOT_FOUND response.
- [v2.314.0 stream pubsub](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pubsub/pubsub.go):
  stream readers reconnect through go-redis; only monitored streams check whether
  Redis lost the stream's initial entry.
- [v2.314.0 executor scheduler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/scheduling/priority_task_scheduler/priority_task_scheduler.go):
  `executor.max_concurrent_tasks` limits execution concurrency while still
  admitting reservations into its queue.
- [REAPI Execution / WaitExecution](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto):
  WaitExecution NOT_FOUND instructs the client to retry Execute. Operation.error
  is forbidden; action errors belong in ExecuteResponse.status. REAPI explicitly
  permits redundant executions, so a duplicate marker alone is a violation of
  this suite's stronger application invariant, not proof of a REAPI defect.

The area-local Docker helper discovers the daemon by inspecting the exact
fixture Redis container, including the harness's private-daemon fallback.
A public harness Docker command method would remove the socket-discovery code.

An early executor-pause experiment deadlocked test admission: the app waits for
an executor reservation acknowledgement before sending the first operation.
The no-Redis-fault control reproduced that test-induced stall. The final tests
use normal single-slot scheduling instead; those earlier failures are not
product findings. With single-slot scheduling, the no-fault control passes.
