# Seeded concurrent Bazel clients

`go_default_test` runs Bazel 8.4.2 on the hosted test runner, against an isolated
released app/executor/cache-proxy fixture. The client binary is downloaded from
releases.bazel.build and checked against its published SHA256 checksum. No client
inherits host credentials, user Bazel config, or a shared service endpoint.

The logged seed determines 3–6 clients, a 6–10-node genrule DAG (including a chain
and diamond), binary input, edits, clean output-base resets, and staggered starts.
Clients use separate workspaces, output bases, and installation bases. Identical
workspace names, commands, paths, and input states make actions overlap between
clients. Even clients use the app cache; odd clients use the proxy cache; all
execution goes to the local app. Local fallback is disabled, and execution logs
check that recorded genrule spawns use a remote runner with successful exit codes.

Each round first builds a reference with a solo Bazel client using local execution
and no remote cache. Every output of every concurrent client must match that
reference byte for byte. The default three rounds cover a shared initial state,
interleaved edits while two clients retain the old state, and clean builds of the
original state. Later rounds repeat varied edits and reverts. The eight-minute
workload deadline bounds extended runs; fixture startup has its own harness bounds.

Run and replay (only on hosted runners):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-multi-client/... --config=nobug --nocache_test_results
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-multi-client/... --config=nobug --test_arg=-multi.seed=271828 --test_arg=-multi.iterations=4 --nocache_test_results
```

Correctness basis:

- [REAPI ActionCache contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto): cached results are addressed by action digest; Execute may reuse results for the same Action. Concurrent uploads of identical data are supported. Deterministic genrules should therefore produce the same bytes independently of other clients and cache routing.
- [Released v2.314.0 execution server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/execution_server/execution_server.go): `getActionResultFromCache` validates cached ActionResults before reuse. The fixture uses the digest-pinned v2.314.0 images documented in `docs/environment.md`.

This is bounded randomized regression coverage, not exhaustive scheduling coverage.
Actual goroutine, process, and server scheduling is nondeterministic; the generated
workspace and requested interleaving replay from the seed. A failing seed needs
repeat runs and a minimized deterministic repro before a finding is filed.

Validation on hosted runners:

- [Default seed, three rounds](https://app.buildbuddy.io/invocation/f9c96631-dcbb-4f5f-b93e-2f8640b11a35): passed in 118 seconds.
- [Seed 271828, six clients, four rounds](https://app.buildbuddy.io/invocation/403550a8-1feb-4b6c-861c-fc1698ddd535): passed in 131 seconds, including the clean-build spawn-count assertion.

No property violations were observed in these runs; no bug-tagged repro or finding
is included.
