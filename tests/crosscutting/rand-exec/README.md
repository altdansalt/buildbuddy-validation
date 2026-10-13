# Random execution properties

The fixture runs the digest-pinned v2.314.0 enterprise app and executor, with
fixture-local Redis, CAS and action cache. The local reference is a shell process
on the remote test runner, not a BuildBuddy component running on the worker VM.

Each seeded case varies quoted arguments, environment values (including empty
strings, quotes, whitespace and UTF-8), input contents, operation count/order,
output declarations, executable bits and successful/nonzero exit status. The
safe vocabulary is `printf`, `cat`, `mkdir`, `chmod`, `ln -s`, and `exit`. Outputs include
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

Round 2 adds eight concurrent, seeded actions in each of bare and Docker isolation.
The payload and environment remain replayable; generated text is never shell syntax.
These actions cover direct file and dangling symlinks, symlinks inside output Trees, and a file declared both directly
and beneath an output directory. Two fresh executions and a cache hit must match
the runner-local reference. Duplicate paths from overlapping declarations must
agree in bytes and executable bits. Symlinks are compared by literal target,
including directory symlinks; the oracle uses Lstat and does not dereference them.

Action-key separation checks every defined Action field: command_digest,
input_root_digest, timeout, do_not_cache, salt, and platform. A sentinel ActionResult
is placed under the original digest; each single-field mutation must miss it,
execute correctly, and leave the original sentinel intact. Cacheable variants
must retrieve their own result; do_not_cache must leave no entry. Equal observable
outputs for different actions are permitted by REAPI and are not evidence of
incorrect sharing. This oracle checks cache-key identity instead.

The area owns a private Docker daemon for each fixture, configured with the public
Google Docker Hub mirror to avoid unauthenticated Hub pull limits. Docker actions
use mirror.gcr.io/library/alpine:3.21.3 with a test-local action network and matching
host/executor workspace mounts. App/executor images remain the pinned releases.
Daemon and container logs are saved as undeclared outputs. No components run on
the worker VM. The daemon helper is adapted from the prior Docker isolation area.

The minimal overlapping-directory bug target is run separately; see
[the finding](../../../findings/executor-rand-exec-overlapping-directory-tree.md):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-exec:overlapping_directory_bug --test_tag_filters=bug --nocache_test_results
```

Deliberately excluded: nondeterministic commands, Firecracker, identical-action
in-flight merging, and timing or resource usage equality.

Round 2 validation: [green ordinary suite](https://app.buildbuddy.io/invocation/25d55482-26e3-4a35-bbdf-67e1c0c5d98e),
[overlap repro 1](https://app.buildbuddy.io/invocation/2774dad2-4e11-4379-876e-86688cf1d9d7),
and [overlap repro 2](https://app.buildbuddy.io/invocation/da25d06f-1124-4fe0-983f-81eb421828a1).
