# Random tree round trips

`TestRandomTreeRoundTrip` starts only a fixture-local app and executor using the
harness's digest-pinned enterprise-v2.314.0 images. Sixteen seeded samples run
sequentially; each RPC/action has a deadline. Samples vary branching and depth,
Unicode normalization forms, whitespace/newlines, quotes, backslashes, leading
hyphens, binary and empty files, duplicate contents, executable flags, empty
directories, and relative file/directory/dangling symlinks.

The action enumerates and hashes its input using standard shell/coreutils,
encoding paths and symlink targets in base64 to avoid delimiter ambiguity. The
Go reference model compares that manifest with the submitted tree. The action
then copies the input with `cp -a`; the test independently resolves the returned
REAPI Tree, reads every output blob, checks its digest, and compares structure,
contents, executable bits, and link targets. Timestamps and other optional node
properties are deliberately outside the equality contract.

Run:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-trees/... --config=nobug
```

Replay/extend with `--test_arg=-trees.seed=314159` and
`--test_arg=-trees.iterations=16`. Failures include both seed and iteration in
the subtest name; replay the full prefix through that iteration rather than
filtering individual subtests. Larger iteration counts may require an increased
Bazel timeout. If a property fails, first preserve its seed, then remove sibling
nodes/subtrees, shorten names/content, and simplify link targets while retaining
the failure; encode the reduced tree in a separate deterministic bug target.

Correctness references:

- [REAPI Directory canonical form and SymlinkNode](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto).
- [Released dirtools implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go):
  `DownloadTree` creates symlinks with their supplied targets; output traversal
  uses directory entries without following links, `handleSymlink` preserves
  targets, and file upload records executable bits (`mode & 0111`).

Bare actions run inside the released executor container; Docker actions run in
their own tool containers. All component tests run on the remote runner.

## Round 2: concurrent trees and Docker

`TestConcurrentOverlappingTrees` runs sixteen replayable batches in each of bare
and Docker isolation. Each batch submits four distinct actions concurrently:
two copies of the same random tree under `rand-trees/instance-a` and
`rand-trees/instance-b`, plus a near-identical tree sharing subtrees and blobs but
changing bytes at the same file path. Instance A requests `OutputFiles` and
`OutputDirectories`; instance B requests `OutputPaths`. Both use the same Go
reference model for the input manifest and returned output Tree. Each action has
a unique command digest, bypasses cache lookup, and disables action caching.
Execution metadata must show at least one pair of overlapping execution intervals
in every batch. A two-second pause keeps small trees in flight together. The copy uses
`mkdir -p output; cp -a input/. output/` so precreated legacy output directories
and initially absent `OutputPaths` directories produce the same structure.

Random trees are generated before starting parallel subtests, so scheduling does
not affect the seed. Replay the full batch prefix with the same seed and iteration
count. The original sequential test remains coverage for repeated materialization
on a warm executor. The default suite runs 144 actions (16 sequential plus 64 per
isolation type). Each concurrent RPC context is bounded to two minutes and each
action to forty seconds.

Docker coverage starts a disposable private Docker daemon inside the test on the
remote runner, exposes its socket only to the fixture executor, and binds its
workspace at the same host/container path. The action tools image is Debian
bookworm-slim, pinned to linux/amd64 manifest
`mirror.gcr.io/library/debian@sha256:a4672c0cb26fbdde88e38fa2dfb6c681942306680e41e4378b28770b6e79ee91`.
It provides GNU coreutils/findutils for the manifest script and has no service
entrypoint. The executor uses an empty Docker client config for anonymous public
image pulls. The BuildBuddy components retain the harness release pins. Daemon
logs are preserved as `rand-trees-dockerd.log` alongside harness artifacts. No shared
component deployment or worker-local component execution is used.

Validation on enterprise-v2.314.0 (harness-pinned app/executor digests):

- [Two-batch setup validation](https://app.buildbuddy.io/invocation/be3195b8-0798-458c-a71e-2260cf0e35e2): passed.
- [Default 16-batch suite](https://app.buildbuddy.io/invocation/d0ae4318-3e43-4e1e-8e12-8e25e255a0ec): all 144 actions passed; every batch in both isolation modes reported six overlapping execution pairs. Command: `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-trees/... --config=nobug --test_output=all --test_arg=-test.v`.

No product defect was reproduced. Setup failures involving copy destination
precreation, private-daemon networking, registry credential helpers, and the
service image entrypoint were corrected before recording passing coverage.
