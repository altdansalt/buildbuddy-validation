# Executor output conformance

These tests execute shell actions against only the released app/executor started
by the fixture, bypassing the action cache. Output bytes are fetched from the
fixture-local CAS and checked against their SHA256 digest; directory Trees are
also decoded and their child files fetched.

Contract references:
- [REAPI Command and ActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto):
  output selection, legacy type checks, symlink representation and missing outputs.
- [Released output implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go):
  NewDirHelper, handleSymlink and UploadTree.
- [Released workspace](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go):
  CreateOutputDirs and UploadOutputs.

`regression` covers modern/legacy files and Trees, missing outputs, empty files
and directories, no declared outputs, executable permissions, content digest
independence from mtime, dangling/file/directory symlinks, an 80 MiB output,
legacy selection overridden by output_paths, and outputs overwriting inputs.
It does not assert that optional requested mtime metadata is supported.

Run passing coverage with:

```sh
bb remote test //tests/executor/outputs/... --config=nobug
```

Bug targets use separate test binaries so failures do not hide passing
coverage. Three findings in findings/executor-outputs-*.md document two reproduction
runs each, with status unverified for maintainer review. Run explicitly with `--test_tag_filters=bug --nocache_test_results`.

## Round 2

`round2` adds four concurrent actions reusing output names with distinct content,
including 12 MiB streaming files and directory markers. It also repeats an
80 MiB changing-output stress case three times: a detached, stdio-redirected
writer changes the first 8 MiB for a bounded five seconds after shell exit.
An execution error is permitted for this unstable output; every successful
result must still reference readable CAS bytes matching its SHA256 digest.
This is scheduling-dependent stress coverage, not a guarantee of snapshot
semantics or of detecting every possible upload race.

Three additional `bug` targets have twice-reproduced findings with status
unverified: `requested_mtime` (successful request silently omits metadata;
explicit INVALID_ARGUMENT is accepted), `overlapping_file` (explicitly declared
nested file missing from OutputFiles), and `overlapping_directories` (parent
Tree missing the separately declared child's Directory message). The latter
was first observed in `round2` and then isolated into its own target. Passing
single-directory nested Trees remain covered by `regression`.

The round 1 PR was still open when this branch was created, so round 2 starts
from the previous area branch and retains its coverage/findings.

Round 2 passing suite: [BuildBuddy invocation](https://app.buildbuddy.io/invocation/fbec580c-729e-4ae5-897d-fbddbc9e7574).
