# Bazel 8 remote flags

`go_default_test` runs checksum-pinned Bazel 8.4.2 inside the test runner. All
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
construction. Default runtime is bounded by four cases and 100 seconds per build.

Replay or expand on a hosted runner:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/bazel-flags/... --config=nobug --nocache_test_results --test_arg=-flags_seed=83142 --test_arg=-flags_iterations=4
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

Retry settings are covered on a real restart, but the randomized eviction retry
setting does not itself force CAS eviction. A forced eviction scenario and
negative authentication cases remain follow-ups.

Validation: [passing default suite](https://app.buildbuddy.io/invocation/bb2c8e03-5b79-40f9-9c99-a2c757fbed89)
(seed 83142, four cases; 234 seconds). No product findings were observed.

The restart workaround discovers the daemon containing the fixture's exact
container name. A harness `Restart`/`Docker` method would remove this dependence
on the private daemon socket naming convention. No shared changes are required
to run these tests. For larger iteration counts, raise the outer `--test_timeout`.
