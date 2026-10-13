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
legacy selection overridden by output_paths, intentional legacy-only directory
precreation, and outputs overwriting inputs.
It does not assert that optional requested mtime metadata is supported.

Run passing coverage with:

```sh
bb remote test //tests/executor/outputs/... --config=nobug
```

`legacy_directory_precreation` reproduces only the workspace change caused by
legacy output_directories when output_paths is also set.

Bug targets use separate test binaries so failures do not hide passing
coverage. Three findings in findings/executor-outputs-*.md document two reproduction
runs each, with status unverified for maintainer review. Run explicitly with `--test_tag_filters=bug --nocache_test_results`.
