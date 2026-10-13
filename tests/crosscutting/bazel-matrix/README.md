# Real Bazel version matrix

`matrix_test` runs checksum-pinned Bazel 7.6.1, 8.4.2 and 9.0.0 release
binaries against a fixture-local enterprise-v2.314.0 app, executor and cache
proxy. Each version exercises local execution with remote cache, and remote
execution through the app with cache reads/writes through the proxy.

The dependency-free Starlark action produces a tree containing a nested input
copy, an empty file, a relative symlink and an executable. Every build checks
bytes (including NUL and non-UTF-8 bytes after an edit), symlink target and mode.
Remote execution also checks the producing container's hostname. Cold and
edited builds reuse an output root, as does a no-op incremental build. Separate
fresh output roots must report remote cache hits for initial, edited and reverted
inputs, so local action cache reuse cannot satisfy those checks.

Run only on hosted runners:

```
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/bazel-matrix/... --config=nobug
```

Bazel binaries are downloaded from releases.bazel.build with fixed SHA256
checksums. Nested Bazel receives an explicit environment allowlist and ignores
all rc files; only disposable fixture API keys and loopback component addresses
appear in build flags. Per-phase Bazel logs and fixture container logs are
preserved in undeclared outputs.

Correctness references:

- [REAPI Action cache contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto): action identity determines reusable results; output trees preserve file contents, executable bits and symlink targets.
- [Pinned release output upload implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go), `UploadOutputs`, delegates output collection to `dirtools.UploadTree`.
- [Pinned release runner](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/runner/runner.go): the bare runner executes commands inside the fixture executor container.

The suite does not cover language toolchains, nested Docker/Firecracker isolation,
or all patch releases.

Round 2 adds `TestSharedProxyCache`: a seed-20261012 binary payload is built
with Bazel 7, then rebuilt with 8 and 9 against the same proxy instance. Each
rebuild must preserve tree contents, empty files, symlinks and executable
permissions. The execution JSON logs record REAPI action digests; matching
digests must report remote cache hits. New digests may legitimately miss. A fresh
Bazel 7 client must still hit its original result after the upgrades. Concurrent cold-upload and warm-read waves of all three versions
use independent workspaces and fresh output roots against the proxy.
Logs are named by phase and version in the fixture outputs.

Bazel version changes do not guarantee identical remote action identities.
[Bazel 7.6.1 command serialization](https://github.com/bazelbuild/bazel/blob/7.6.1/src/main/java/com/google/devtools/build/lib/remote/RemoteExecutionService.java#L246)
sets both legacy output fields and `output_paths`, whereas
[Bazel 8.4.2](https://github.com/bazelbuild/bazel/blob/8.4.2/src/main/java/com/google/devtools/build/lib/remote/RemoteExecutionService.java#L258)
selects one representation based on server capabilities. A version-dependent
miss alone is not a cache correctness defect. The concurrent warm wave requires
a hit for every version's action identity populated by the sequential builds.
