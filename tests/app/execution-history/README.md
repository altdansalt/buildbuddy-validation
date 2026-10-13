# Execution history

`go_default_test` runs bounded seeded shell actions against the released local app
and executor, with a live BES invocation. It covers successful/nonzero-exit
commands, execution deadlines, and direct action-cache hits; compares history
against terminal REAPI responses; checks uniqueness, digest, status, exit code,
worker and ordered timestamps; and checks ID/digest filters before and after BES
completion. Replay with `--test_arg=-history.seed=314159`; increase cases with
`--test_arg=-history.iterations=40` (maximum 40, four-minute RPC budget).

`target_label_filter_bug` uses two actions and asks the documented target-label
filter to return only one. Run explicitly with `--test_tag_filters=bug` and
`--nocache_test_results`.

Oracles inspected at release v2.314.0:

- [execution_stats.proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/execution_stats.proto): lookup fields and history semantics.
- [execution service](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/execution_service/execution_service.go): primary-DB and OLAP lookup paths, inline response retrieval.
- [execution server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go): direct AC hits return before insertion, so these do not count as scheduled executions. The hit retains the original worker metadata.
- [REAPI](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto): status/exit-code distinction and metadata timing order.
- [v1 service](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/api/v1/service.proto): no execution-list RPC; GetAction describes BES actions rather than scheduled executions.

No ClickHouse is enabled; this exercises the supported default primary SQLite
history path. Concurrent execution, merged executions, OLAP and executor-side
cache hits remain follow-ups. BES uses a minimal wire-encoded Started/Finished
stream from the release proto; no source build or generated BuildBuddy dependency
is required.

Validated default suite:
https://app.buildbuddy.io/invocation/7d82b2f9-80e6-432f-932d-2ba731597417.
Target filter finding and both reproductions:
[app-execution-history-target-label-filter.md](../../../findings/app-execution-history-target-label-filter.md).
