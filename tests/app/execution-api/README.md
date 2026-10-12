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
