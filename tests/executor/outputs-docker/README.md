# Executor outputs under Docker isolation

Ports the output cases from PRs #23/#60/#74 (area/executor-outputs,
area/executor-outputs-2 and area/executor-outputs-3) to released v2.314.0.
Actions use `workload-isolation-type=docker` and digest-pinned Alpine 3.24.2
from `mirror.gcr.io`. Each fixture owns a private dockerd and mounts its socket
and workspace into the executor. The workspace path is identical on the daemon
host and executor. No Docker Hub pulls, shared component endpoints, or source
builds are used.

`regression` checks modern and legacy files, missing outputs, empty outputs,
executable bits, nested directory Trees, dangling/file/directory symlinks, an
80 MiB output, overwriting inputs, legacy directory compatibility precreation,
and modern selection superseding legacy fields. `round2` checks four concurrent
uploads sharing output names and bounded mutation during upload; successful
results must always have readable CAS bytes matching their digests. Docker may
terminate the detached writer on container exit, so mutation is stress coverage,
not a requirement that the writer survives.

`randomized` generates sorted, unique, valid modern or legacy declarations,
files with varied bytes and executable permissions, nested Trees with empty
directories and dangling symlinks, missing files, and undeclared files. It checks
an independent reference model and compares the same commands under bare and
Docker isolation. The default seed is 314159 with 24 iterations (48 actions).
Every subtest name contains its replay seed and iteration. Declarations are
disjoint here; known overlapping-declaration defects have fixed repro targets.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker:randomized --test_arg=-outputs.seed=42 --test_arg=-outputs.iterations=100
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker/... --test_tag_filters=bug --nocache_test_results
```

RPC contexts have an eight-minute suite budget and each action has a one-minute
timeout. Increase the suite context in this directory if doing exceptionally
long seed runs. Each bug is a separate target and finding so passing coverage
is not hidden by an expected failure.

Contract references:
- [REAPI Command/ActionResult/Tree](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).
- [Released dirtools](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go).
- [Released workspace](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go).

Docker reproduction results and invocation links are recorded below after runner
validation; findings keep status `unverified` for maintainer review.
