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

## Round 2: mixed versions and cancellation

`TestMixedVersionsCancellation` downloads checksum-verified Bazel **7.6.1, 8.4.2,
and 9.0.0** and runs six clients against one disposable app/executor/proxy fixture.
Each version has one direct-cache client and one proxy-cache client; every client
uses remote execution through the local app, with local fallback disabled. The
proxy does not implement the Execution service. A dependency-free Starlark rule
keeps the generated workload compatible across all three major versions.

The seed determines a 6–10-node chain/diamond DAG, additional edges, binary inputs,
the victim's cache route, and cancellation delay. Each round uses fresh inputs and
fresh output bases. An independent Go oracle computes every payload output; a
separate hostname output proves that results came from the fixture executor,
including results subsequently served from the action cache. Execution logs must
record every action as a successful remote spawn.

The victim starts first. Its first remote action writes a marker to a test-owned
bind mount and sleeps for 20 seconds. After observing that marker, the test starts
the other five clients, waits a seeded 500–1999 ms, and sends SIGINT to the victim
Bazel process. This exercises graceful client/RPC cancellation during remote work;
a 15-second shutdown limit bounds the client process. The marker is an observation
hook, not an output or cache oracle. All survivors must finish with correct bytes,
then the cancelled client retries with a clean output base and must also match.
The first three rounds cancel one client from each Bazel major version. Actual
process and server scheduling remains nondeterministic.

Client stdout/stderr is preserved per round/client/phase alongside the harness
configs, exact image metadata, and component logs in undeclared test outputs.
Use the same seed/iteration flags above to replay both tests. To run only the new
coverage, append `--test_filter=TestMixedVersionsCancellation`.

Round 2 hosted validation on the default digest-pinned enterprise-v2.314.0 images:

- [Focused mixed-version cancellation, default seed](https://app.buildbuddy.io/invocation/ef294316-72b4-4693-9c1b-ba4e86f4f25e): passed in 142 seconds.
- [Full area suite, default seed, final cleanup ordering](https://app.buildbuddy.io/invocation/7a9929f6-1758-4d04-b1b4-8cbb5b2ad76e): passed in 267 seconds. Both the existing concurrent-client regression and mixed-version cancellation coverage passed.
- [Mixed-version seed 271828, six rounds](https://app.buildbuddy.io/invocation/d5c52e03-e0fe-4845-9696-ba7891167b1b): passed in 261 seconds. Cancelled each of the six version/cache-route combinations; all survivors and clean retries matched the oracle.

No product property violation was observed. No bug-tagged repro, finding, or
shared harness/dependency change is needed.
