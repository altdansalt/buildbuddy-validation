# Bounded mixed-load soak

Run `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/soak/... --config=nobug --test_timeout=900 --nocache_test_results --test_output=all`.

The repository currently forces a 600-second timeout globally. Override it to
900 seconds to allow the full ten minutes of load plus fixture startup/cleanup.
The target declares `timeout = "long"`; removing the global override would make
the ordinary area command sufficient. A prestarted daemon
(`bb remote --runner_exec_properties=init-dockerd=true test ...`) can avoid
Docker Hub Redis pull limits when its image is already cached.

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

## Validation

[Passing v2.314.0 run](https://app.buildbuddy.io/invocation/0d3978e4-3e1b-41c9-a642-095d7d2ceeec)
completed the full load with 719 mixed operations and no RPC/data errors. Early
and late cache medians were 7.98/7.93 ms; execution medians were 16.71/15.04 ms.
Goroutines and descriptors stayed stable, and executor `/data` stayed at 76 KiB
after warmup despite the scratch writes. App/proxy images do not include `du`,
so their physical volume usage remains a follow-up beyond the exposed metrics.

Environment diagnostics:
- [Redis pull rate limit](https://app.buildbuddy.io/invocation/9e2be086-4455-437d-9ee3-ad87af0179bf)
- [Global timeout override](https://app.buildbuddy.io/invocation/d049bf35-082b-44b4-91b4-f4bfa077b7a4)

No product finding was identified. Longer runs, eviction/GC workloads and full
app/proxy volume accounting remain follow-ups. A harness helper exposing fixture
volume usage without requiring utilities inside the image would improve coverage.
