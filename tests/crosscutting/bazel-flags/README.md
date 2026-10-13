# Bazel 8 remote flags

All targets run checksum-pinned Bazel 8.4.2 inside the test runner.
`go_default_test` covers restart and eviction; the three `seed_*_test` targets
cover the randomized flag matrix. All
cache and execution endpoints address harness-local released containers. Nested
Bazel uses an empty environment and ignores every rc file; the dispatch API key
is never forwarded.

The seeded property combines instance names (including empty and nested), Merkle
tree caching, fallback, retry counts, timeouts, eviction retry limits, extra
headers and regex-based downloads. Bazel 8.4.2 removed
`--incompatible_remote_symlinks` (both values are rejected); symlink semantics
are covered through the input and output symlinks instead. Cases alternate between
app/executor and proxy cache builds. Every cold and warm build uses a fresh Bazel
output root. Binary outputs, an intermediate selected by the download regex,
and a relative output symlink are compared with a local reference build. Warm
builds must report real remote-cache hits. An input symlink exercises input tree
construction. Each seed runs six cases with a 100-second bound per nested build. The suite
runs seeds 83142, 20261013 and 917003 in separate Bazel targets. Download modes,
cache compression and local-result upload settings extend the flag matrix.

Replay or expand on a hosted runner:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/bazel-flags/... --config=nobug --nocache_test_results --test_arg=-flags_seed=83142 --test_arg=-flags_iterations=6
```

`flags_iterations` accepts 1 through 24. Every case logs the replay seed and flags
with its disposable key redacted. A separate fixed case restarts the local app
while the executor is running a delayed action; retries, a short RPC timeout and
local fallback must preserve the local reference bytes.

Correctness references:

- [Pinned BuildBuddy AC implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go)
  validates output references before returning a cached result.
- [REAPI](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
  defines content digests, ActionResult symlink targets, and implementation-defined
  instance names. These tests do not assume instance names isolate organizations.
- [Bazel 8.4.2 remote options](https://github.com/bazelbuild/bazel/blob/8.4.2/src/main/java/com/google/devtools/build/lib/remote/options/RemoteOptions.java),
  [common options](https://github.com/bazelbuild/bazel/blob/8.4.2/src/main/java/com/google/devtools/build/lib/remote/options/CommonRemoteOptions.java),
  and [execution options](https://github.com/bazelbuild/bazel/blob/8.4.2/src/main/java/com/google/devtools/build/lib/exec/ExecutionOptions.java).

## Round 2 eviction coverage

`TestForcedCASEviction` uses the released app's disk cache instead of Pebble so
CAS files can be removed deterministically while preserving AC files, SQLite
and authentication state. It checks a warm AC hit and a successful CAS read,
uses a network-disabled helper with the already-pulled released executor image
to delete only files outside `/ac/`, verifies a nonzero unchanged AC file count,
restarts the app to refresh its disk-cache index, refreshes Docker's randomly
published gRPC port, and waits for FindMissingBlobs to report the CAS digest
missing.
A fresh-root build must then recover without local fallback, preserve exact
output bytes and the relative symlink, and repopulate a usable remote cache.

The matrix covers `all`, `toplevel` and `minimal` with eviction retry budgets 0
and 2. Minimal downloads select only final outputs via regex. Only `all` may
materialize the intermediate. This exercises stale-AC validation after eviction
between builds; it does not force the narrower race of eviction after AC lookup
inside a build, so passing with budget zero does not prove Bazel exercised its
cache-eviction retry path. Proxy flags remain covered by the seeded matrix;
forced eviction targets the app/executor path to avoid a second CAS copy in the
proxy masking the missing backend blob.

Replay the full suite on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/bazel-flags/... --config=nobug --nocache_test_results
```

PR #63 (harness lifecycle controls) was still open when this branch was created,
so the fixture-scoped Docker workaround remains. The earlier area PR #84 was
also open at branch creation and merged during validation; round 2 branches
from its head. No shared edits are required.

Validation: [focused eviction pass](https://app.buildbuddy.io/invocation/3a62c193-1116-47f3-9372-908c42532b01)
(all six combinations, 178.9 seconds). Setup failures were resolved by using a
shell-bearing volume helper for the distroless app and refreshing published
ports after restart. No product defect was observed.
