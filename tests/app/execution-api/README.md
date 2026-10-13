# Execution API conformance

All RPCs address an authenticated, disposable harness app. Stream tests also
start a released bare-runner executor; validation tests intentionally need no
executor because they fail before scheduling.

`go_default_test` covers missing action/command status mapping, rejected execution
priorities outside the released range, malformed WaitExecution names, stable
operation names and action metadata, terminal response/EOF, reconnecting after
Execute cancellation, two concurrent waiters, completed-operation replay,
results cache policy hints, cache-hit responses, and nonzero shell exit mapping.
Cache priorities are implementation-defined hints; these tests do not claim to
measure eviction ordering. The stream tests use both accepted execution priority
boundaries and do not assume scheduling order between unrelated actions.

`missing_command_bug_test` verifies that the PreconditionFailure MISSING subject
identifies the absent command. FindMissingBlobs first confirms that the action
and input root are present, ruling out an upload/configuration error.

References:
- [REAPI Execution contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
- [Released execution service](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go)

Run passing coverage with `bb remote test //tests/app/execution-api/... --config=nobug`.
Run the reproducer explicitly with `--test_tag_filters=bug --nocache_test_results`.
Follow root AGENTS.md for the public runner-fetch remote.

Round 2 adds replayable policy/cache and queue coverage (seed `20261013`):

- Every results-cache priority / skip-lookup combination, with and without
  Action.do_not_cache, in seeded order; cacheable duplicate in-flight calls
  share an operation, while do_not_cache calls execute independently.
- A single occupied executor slot gates three distinct priorities. Executor
  reservation logs establish that all three are queued before release, and
  the actions record their execution order in a fixture-local bind mount.
- A 4 MiB Pebble cache receives seeded pressure with eviction enabled. Large
  action-cache entries at both priority boundaries must disappear, then Execute
  must run again and repopulate the cache. Eviction observation order is logged;
  no total priority eviction order is asserted because REAPI makes retention
  priority semantics server-dependent. Restoring inputs rules out missing CAS.
- Completed WaitExecution replay, a valid unknown name, and an expired stream
  with Redis availability monitoring enabled. Expiry is accelerated by shortening
  only the fixture's operation stream TTL after verifying completed replay.
  Docker commands locate the daemon owning that fixture and address only its
  containers, including when the harness owns an isolated dockerd.

`unknown_operation_bug_test` is a separate minimal default-configuration repro:
valid unknown names hang until the client's five-second deadline. It reproduced
on two fresh fixtures; the round 2 finding remains `unverified` as requested.
