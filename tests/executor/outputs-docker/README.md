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

## Bare finding comparison

All six reported bare findings also reproduce under Docker. Status stays
`unverified` for maintainer review; each has two successful-action reproductions.

| Bare finding | Docker target | Result | Severity |
| --- | --- | --- | --- |
| Legacy type mismatch | `legacy_type_mismatch` | Same incorrect status/types in all four cases | medium |
| Modern symlink lost with legacy fields | `output_paths_precedence` | Same missing symlink | medium |
| Legacy directory precreation | `legacy_directory_precreation` | Same ignored-field workspace mutation | low |
| Requested mtime omitted | `requested_mtime` | Same missing file metadata | low |
| Explicit nested file omitted | `overlapping_file` | Same absent OutputFile (reduced to two declarations) | low |
| Overlapping directory Tree | `overlapping_directories` | Same missing child Directory | medium |

Legacy-only precreation also occurs under Docker, retained as passing coverage
for the release's explicitly documented old-Bazel compatibility behavior. The
bug target isolates legacy directory creation when modern output_paths should
supersede the legacy fields. Requested-mtime coverage accepts explicit
INVALID_ARGUMENT, allowing implementations to decline unsupported metadata.

Validation:
- [Passing suite: all three targets](https://app.buildbuddy.io/invocation/b178eaf6-d3a8-4d00-9dab-9fe5703df28d), including seed 314159, 24 iterations per isolation mode.
- [Bug run 1](https://app.buildbuddy.io/invocation/d6f2abde-f559-4a6b-8ca8-71d18c9960f9) and [bug run 2](https://app.buildbuddy.io/invocation/1c11d133-6aef-412d-b0a1-59b5404def25): five valid output failures; overlapping_file had a shell setup error here and is excluded.
- Corrected minimal overlapping-file [run 1](https://app.buildbuddy.io/invocation/d1eb4d81-c8c8-45c7-8dd9-74d5e386c813) and [run 2](https://app.buildbuddy.io/invocation/dbf60ba2-6805-4d6f-b644-b90385e63425): successful actions, expected missing-OutputFile failures.

Individual reports are `findings/executor-outputs-docker-*.md`. Initial Redis
readiness failures from disabling private-daemon iptables were setup failures,
not product findings. The area fixture now enables network rules and serializes
its entire lifetime with the existing harness daemon lock. This is a local
workaround; a reusable Docker-isolation fixture would be a useful harness addition.
