# Invocation cache statistics

`//tests/app/cache-stats:go_default_test` starts the pinned released app and
executor inside the remote test, enables `cache.detailed_stats_enabled`, and
uses a checksum-pinned Bazel 7.4.1 client against their published loopback ports.
The client has a fresh output root for each build and a minimal environment
without the outer runner's dispatch credentials. Its workspace uses native
`genrule` targets, with no external dependencies.

Each logged seed generates 4–8 targets with random input contents/sizes. Builds
populate the cache locally, hit the warm cache, then change a random nonempty
proper subset and execute the misses remotely. Execution-platform properties
remain identical across all three builds to preserve unchanged action digests.
Every output is checked against the test's independently generated contents.
The test checks:

- Bazel's JSON execution log against the expected hit/miss mix and remote count.
- Action digests, targets and hit status against individual scorecard requests,
  and aggregate AC hits/misses/uploads against the generated workload in all phases.
- Output digests and byte sizes against successful scorecard CAS transfers.
- Aggregate request counts, logical bytes and transferred bytes against the
  complete paginated scorecard for cache-only builds.
- Independently counted CAS uploads, repeated downloads and a missing read,
  including empty, one-byte, 4 KiB, 64 KiB + 1, and random payloads. Repeated
  one-byte reads force the scorecard past its 100-row page boundary.
- The invocation's execution API record count against Bazel's remote executions.

The released implementation intentionally excludes executor requests from the
scorecard while retaining their transfers in aggregate cache stats. Therefore
remote-build CAS totals use a lower-bound check against the client scorecard;
this test does **not** claim an exact independent count of executor-internal
transfers. Exact accounting of that traffic is a follow-up requiring a local
instrumented executor cache endpoint. Cache-only totals are checked exactly.
The test polls for asynchronous scorecard/stat persistence after BES completion.
Execution logs and API snapshots are saved in undeclared outputs.

Run only on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cache-stats/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cache-stats/... --config=nobug \
  --nocache_test_results --test_arg=-cache_stats.seed=42 \
  --test_arg=-cache_stats.iterations=3 --test_arg=-test.v
```

Iterations are bounded to 1–5; each Bazel build has a two-minute deadline, probes
have a 30-second deadline, and scorecard pagination is bounded. The default
three-build workload takes roughly one minute including fixture startup.

Contracts consulted:

- [Released CacheStats and ScoreCard protos](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/cache.proto)
- [Released hit tracker](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/hit_tracker/hit_tracker.go)
  (`recordDetailedStats`, `CollectCacheStats`, `transferTimer.Record`)
- [Released scorecard pagination](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/scorecard/scorecard.go)
- [Released BES statistics finalization](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_handler/build_event_handler.go)
- [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
  (GetActionResult NOT_FOUND and content-addressed output semantics)
- [Bazel execution-log schema](https://github.com/bazelbuild/bazel/blob/7.4.1/src/main/protobuf/spawn.proto)

Logical byte totals use the requested digest size, including **action digest**
sizes for AC requests. Transferred AC bytes describe serialized ActionResults,
so these two byte fields need not match. Identity-compressed CAS output/probe
transfers must match their digest sizes. The default executor performs an
additional GetActionResult before executing a miss: aggregate AC misses count
both Bazel and executor requests, while the scorecard counts only Bazel requests.
The workload model includes this second miss for each remote execution, per
[released executor source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/executor/executor.go).
