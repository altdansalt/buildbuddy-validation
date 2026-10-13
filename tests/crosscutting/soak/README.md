# Bounded mixed-load soak

Run `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/soak/... --config=nobug --nocache_test_results --test_output=all`.

The released app, proxy and bare executor run in one private harness fixture.
Each execution creates a 4 MiB scratch file to exercise workspace cleanup.
Ten minutes of load issue roughly one cache transaction per second and one real
execution every five seconds. Every transaction verifies bytes through both
proxy and app and checks a seeded AC entry. Executions bypass the AC, do not
cache results, and verify fetched output through the proxy.

A fixed corpus avoids expected unbounded storage growth. Loopback-only monitoring
ports provide minute samples of Go heap/goroutines, RSS, open descriptors,
Pebble level sizes and executor pooled workspace disk usage, when exposed.
When `du` is present in an image, total `/data` usage is also sampled.
`soak.json` in fixture undeclared outputs retains samples, operation latencies
and errors. Metrics are summed across label sets. Conservative resource budgets
compare minute two with minute ten; latency compares medians in minutes 1–2 and
8–9. These budgets catch large regressions, not small leaks. Container logs are
checked for Go panics and fatal runtime errors.

This is a steady-state soak, not an eviction/GC or capacity test. Disk metrics
cover Pebble levels and pooled workspaces; `/data` sampling additionally covers
the volume when available. It does not prove all temporary files are reclaimed or exclude slower leaks beyond ten
minutes. No throughput claim is made.

Behavior references:
- [REAPI ActionCache and Execution contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
- [v2.314.0 runner cleanup and recycling](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/runner/runner.go)
- [v2.314.0 metric definitions](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/metrics/metrics.go)
