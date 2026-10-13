# Executor command environment and arguments

Tests start only fixture-local app, Redis, and released executor containers.
Actions use the bare Linux runner, disable action caching, and have bounded
RPC/action deadlines. Container logs are preserved by the fixture and checked
for Go panic/fatal-error signatures after cleanup.

`regression_test` covers argument ordering, duplicates, empty strings, whitespace,
quotes, newlines, Unicode, sorted environment names, empty values, embedded `=`,
duplicate environment names, a 64 KiB environment value, 2,000 arguments, a
60,000-byte individual argument, concurrent action-local environments, relative
executables, and working-directory rejection/recovery. Duplicate environment
precedence is implementation regression coverage: REAPI does not prescribe
which duplicate wins. Environment names supplied by valid coverage are sorted.

`path_bug_test` is the minimal failing command-PATH reproducer. An executable
input script succeeds through `./envargs-tool` in `tools`, then fails through
`envargs-tool` with `PATH=.` in the same directory.

References:

- [REAPI Command at the scaffold's pinned revision](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto): argument resolution, PATH SHOULD be respected, environment MUST be sorted, working directory must exist in the input tree.
- [Released executor command construction](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/commandutil/commandutil.go): `exec.Command` performs lookup before action environment assignment.
- [Released working-directory tree validation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go).

Run on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/env-args/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/env-args:path_bug_test --test_tag_filters=bug --nocache_test_results
```

An explicitly pushed branch can be selected with
`--run_from_branch=area/executor-env-args` before `test` to bypass local workspace
mirroring. No harness or dependency modifications are needed.

Round 2 adds `round2_test`, which runs the same seeded fidelity and execve limit
checks under bare and Docker isolation. Replay with
`--test_arg=-envargs.seed=20261012`; each failure identifies its case index.
Twenty-four cases combine sorted environment maps and argv containing empty
strings, Unicode, equals signs, whitespace, shell metacharacters, and long values.
The oracle compares NUL-delimited output byte-for-byte; generated names are safe
POSIX identifiers so the shell probe does not alter their meaning.

Limit coverage checks 131,000-byte successful strings, rejects 131,072-byte
strings and 2.4 MB aggregate argv without a timeout, then verifies recovery.
Docker actions use Alpine 3.21.3 and a private test-owned daemon, socket and
workspace; released app/executor images retain the harness digest pins.
PATH coverage includes qualified names with an unusable PATH, system tools,
and Docker relative/missing/empty PATH entries. Bare action-PATH lookup cases
extend the existing `bug` target and finding rather than duplicating the defect.
