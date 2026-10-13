# Execution state conformance

Run `bb remote test //tests/statemachine/execution-state:go_default_test`.
The isolated regression target requires `--test_tag_filters=bug --nocache_test_results`.
All product requests address disposable fixture-local released containers.
Images are the default digest-pinned enterprise-v2.314.0 app/executor/proxy;
exact image digests and logs are preserved by the harness.
The real CLI is release 5.0.492, SHA256
`e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.

## Mutation and surface inventory

| Mutation | Checked reads | Result |
| --- | --- | --- |
| REAPI Execute, repeated Execute, forced re-execute | Original stream; both gRPC WaitExecution APIs; UI HTTP WaitExecution; full HTTP/gRPC GetExecution rows; listing cardinality/action filter/exclusion; two API keys in one org | Fresh uncached execution IDs and deterministic output; cache hit returns matching ActionResult |
| Executor PublishOperation (indirect) | Above terminal reads; action-key and execution-ID-keyed app/proxy AC; ByteStream and FindMissingBlobs; BatchReadBlobs; REAPI GetTree; BuildBuddy HTTP/gRPC GetTree and GetTreeDirectorySizes; UI /file/view and /file/download; GetExecutionDownloads; execution-profile download; bb download and bb execution get | Terminal result agreement and stable repeated observations; each intermediate publication is not controlled |
| CancelExecutions via HTTP and separately via gRPC, then repeated cancellation | Executing HTTP/gRPC projections; original terminal stream; both gRPC waits and HTTP wait; full GetExecution; execution-ID AC on app/proxy; UI response and profile downloads; GetExecutionDownloads; bb execution get; action-key AC absence on app/proxy | CANCELLED, terminal result stable, cancellation cannot create a cache hit even with DoNotCache=false |
| Cache-hit Execute → REAPI WaitExecution | Completed Execute versus a new wait stream for its exact returned name | Confirmed missing replay; isolated bug target |

The audit's execution-ID AC/UI response-download probe was imported with its
original expectations; no audit perturbations were retained.

## Reference model and sequences

The independent model uses invocation ID, submitted action digest, immutable
operation identity, monotonic stage, command exit/status semantics, exact output
path/digest/content, and stdout bytes. Timing fields come from the real executor
stream; their cross-surface agreement is checked rather than inventing expected
wall-clock timestamps. Full standard REAPI result/response comparisons include
status, exit code, digest sizes, stdout/stderr, and standard metadata.

The success sequence is execute→cache-hit execute→forced re-execute of the same
action. Forced re-execution must allocate a fresh ID and preserve command output.
Final listing queries assert both execution IDs, exact cardinality, action-hash
filtering, and exclusion of a different valid hash. Input CAS is written through
proxy and consumed by the real executor/app; app-produced output is read through
both app and proxy.

HTTP protobuf GetExecution rows must equal their gRPC counterparts in every
serialized field, with exactly one row for an ID lookup. A second same-org API
key must return the identical projection. Top-level status, exit code, response
digest, worker and timestamps are checked against inline state. SQL projections
store microseconds and materialize unset timestamps as Unix epoch; this specific
representation is normalized based on TableExecToClientProto. CLI JSON excludes
auxiliary metadata before comparing standard fields, since those BuildBuddy Any
types are not generated in this repo. The release also trims auxiliary/unknown
execution-metadata extensions from REAPI streams and ordinary AC responses.

Each terminal state has three complete projection/cache/wait observations,
250ms apart. Action-key AC is separately checked three times per component.
Bounded convergence polling allows 30 seconds. Logged GetExecution timings are
sampling upper bounds from the start of that read check, after terminal receipt
and API-key provisioning; polling uses 100ms intervals and does not identify the
exact publication-to-database latency. Stability sampling covers at least 500ms.

HTTP WaitExecution and streaming GetTree use the frontend's actual
application/proto+prefixed framing. UI file reads supply the frontend's base64
group request context. GetExecutionDownloads explicitly asserts empty results
and HTTP/gRPC agreement: the default app does not enable the input-fetch-metadata
experiment. The execution-profile JSON must contain the expected command span
and agree with execution-metadata timestamps/duration (or omit it when timestamps
are absent). The BES cleanup rejects every receive error other than EOF.

## Confirmed divergence

See [cached Execute replay finding](findings/app-execution-state-cached-wait.md).
Two independent remote runs observed no WaitExecution message for 10 seconds
following a completed cache-hit Execute. Source shows that path never publishes
replay state. The bug target retains the expected conformance assertion and is
excluded from normal CI.

## Source investigation and remaining limits

Inspected released proto/{remote_execution,execution_stats,cache,buildbuddy_service}.proto;
enterprise/server/remote_execution/execution_server; enterprise/server/execution_service;
enterprise/server/util/execution; server/buildbuddy_server;
server/http/protolet; server/remote_cache/directory_size;
cli/{download,execution}; and app/{service/rpc_service,invocation/invocation_action_card,invocation/execution_status}.

This is expanded coverage, not exhaustive all-surface conformance. PublishOperation
is produced by the real executor; there is no controlled direct publisher checking
each intermediate publication against every surface. Stable checks cover terminal
state; queued/executing updates cannot be frozen in this fixture, and SQL
intentionally flushes executing state only after five seconds. Complete HTTP/gRPC
row equality includes auxiliary fields between those two reads, but there is no
independent reference oracle for their contents.

SearchExecution/GetExecutionTimeline need OLAP support; populated execution-download
metadata needs an app experiment provider. These require shared fixture extensions.
Queued cancellation, merged executions and retries remain uncovered. There is no
execution delete RPC, and the proxy exposes cache APIs rather than execution
mutations/projections, so execution create/update/delete/re-create and Execute via
proxy do not map to this API. No product requests address shared deployments,
no source builds are performed, and no upstream reports or PRs are created.

## Remote validation

- Normal expanded target passed: https://app.buildbuddy.io/invocation/fbef6d22-f78d-4356-bd30-e5d0179916c3
- Cache-hit replay reproducer failed at its intended assertion twice:
  https://app.buildbuddy.io/invocation/c30db903-56d0-4677-a1c7-7cec274f720f
  and https://app.buildbuddy.io/invocation/44742c06-7944-4fc5-86ac-78c021d2560a
- During development, terminal GetExecution read checks converged in roughly
  103–104ms including the first 100ms polling interval; three repeated projection
  observations covered at least 500ms. This is a sampling upper bound after
  terminal observation, not a measured replication delay.
