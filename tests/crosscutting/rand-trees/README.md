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

The shell runs inside the released executor container, never on the worker.
