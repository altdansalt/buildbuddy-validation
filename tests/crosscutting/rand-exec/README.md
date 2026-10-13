# Random execution properties

The fixture runs the digest-pinned v2.314.0 enterprise app and executor, with
fixture-local Redis, CAS and action cache. The local reference is a shell process
on the remote test runner, not a BuildBuddy component running on the worker VM.

Each seeded case varies quoted arguments, environment values (including empty
strings, quotes, whitespace and UTF-8), input contents, operation count/order,
output declarations, executable bits and successful/nonzero exit status. The
safe vocabulary is `printf`, `cat`, `mkdir`, `chmod`, and `exit`. Outputs include
binary bytes, empty files, nested and empty directories, undeclared outputs and
missing declarations. Both legacy output files/directories and output_paths are
covered. No generated value becomes executable shell syntax.

Two sequential skip-cache executions must agree with the local reference.
Successful cacheable actions also require a populated ActionCache and an actual
Execute cache hit with identical semantic results. Nonzero exits are compared
without requiring caching. Comparison resolves CAS blobs and Tree children,
checks their digests, and ignores execution timestamps and other metadata that
are allowed to vary.

Run:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-exec/... --config=nobug --nocache_test_results
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-exec/... --config=nobug --nocache_test_results --test_arg=-rand-exec.seed=271828 --test_arg=-rand-exec.iterations=32
```

The default is 16 cases and seed 314159. The execution loop has a four-minute
budget, each case a 90-second budget, and each remote Action a 20-second timeout.
Seed and case index appear in subtest names; failures log the full Command and
input. Replay with the same seed and enough iterations to include the failing
index. Stop at the first failing case. For longer runs, raise iterations; the
four-minute execution budget still applies. A discovered violation should be
reduced by deleting script operations, simplifying values and removing output
declarations, then preserved as a separate deterministic bug-tagged target.

Contract and implementation references:

- [REAPI Command and ExecuteRequest](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto): sorted environment and declarations, output selection and skip_cache_lookup semantics.
- [v2.314.0 runner](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/runner/runner.go): task execution and output upload delegation.
- [v2.314.0 workspace](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go): stdout/stderr CAS upload and dirtools UploadTree.

Deliberately excluded: nondeterministic commands, symlinks, overlapping output
roots, Docker/Firecracker isolation, concurrent execution merging, and timing or
resource usage equality. These require separate reference rules or environments.
