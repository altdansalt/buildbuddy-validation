# Metrics consistency

`go_default_test` starts the pinned app, executor, and cache proxy in one isolated
fixture. No component traffic goes to a shared deployment. It publishes their
monitoring port 9090 using area-local `DockerArgs`, since the harness does not
expose monitoring endpoints. Ephemeral loopback ports are reserved then released
just before Docker creates the containers (a small bind race remains).

The seeded workload covers nonempty random blobs around histogram boundaries,
ByteStream uploads, repeated cache hits, missing reads, direct app transfers,
and uncached shell actions with zero and nonzero exit codes. Scrapes run every
100 ms during the workload. Assertions check:

- Exact proxy request, error, and uncompressed byte counter deltas.
- Exact app CAS upload/download histogram observation counts and byte sums.
- Exact executor counts by exit code and execution-stage histogram count/sum,
  using returned execution timestamps as the reference model.
- Stable metric types and label schemas, monotonic counters, cumulative
  histogram buckets, and equality of each histogram's +Inf bucket and count.
- HTTP 200 and valid, duplicate-free samples while components are active.

Before/after scrapes are saved alongside the fixture's logs and image metadata.
The seed is logged and printed again on failure. Example replay/longer run:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/metrics-consistency/... \
  --config=nobug --nocache_test_results \
  --test_arg=-metrics.seed=42 --test_arg=-metrics.iterations=100
```

Iterations are bounded to 1..500, and the RPC/scrape workload has a four-minute
context deadline. There are six actions, including guaranteed successful and
nonzero-exit commands. Nonzero exit is an ActionResult outcome, not an execution
RPC failure. Empty CAS and duplicate-write short-circuits are deliberately
excluded from byte-count oracles.

The contract comes from the released
[v2.314.0 metrics definitions](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/metrics/metrics.go),
[hit tracker](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/hit_tracker/hit_tracker.go),
[ByteStream proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go),
[executor metric recorder](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go),
and the repository-pinned
[Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).
Deferred invocation-level cache statistics are not treated as synchronous RPC
counters. Metric names and selected label schemas are pinned release regression
contracts, not requirements imposed by the Remote Execution API.
