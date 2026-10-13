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

`declaration_property` runs 48 reproducible generated actions (default seed
20261013), rotating modern-only, legacy-only, both two-field modes and all three fields. It
randomizes declarations and creation independently across two nested subtrees,
including overlapping parent/child declarations, absent paths, empty directories,
regular files with randomized executable bits, and file/directory/dangling links.
The oracle compares exact output path sets, link targets and result fields,
CAS-verified file contents, executable bits, and recursively decoded Tree contents.
All declaration lists are sorted and unique and legacy file declarations never
contain an ancestor of another legacy file declaration.

Replay or extend the sequence with:

```sh
bb remote test //tests/executor/outputs:declaration_property --config=nobug \
  --nocache_test_results --test_output=all \
  --test_arg=-outputs.seed=20261013 --test_arg=-outputs.cases=48
```

Each case logs its index and complete shell command/declarations. Existing
findings are narrow oracle exceptions, not whole-case skips: mixed fields use
the known legacy symlink classification/loss; legacy symlink type mismatches
use the already observed successful classification; explicit nested files use
the confirmed omission exception while their bytes remain checked in Trees.
For confirmed incomplete overlapping Trees, missing directory messages can only
be supplied from separately returned descendant Trees with matching digests;
all reconstructed contents are still compared against the independent model.
The command clears executor-precreated directories before explicitly recreating declaration
parents in the generated command and its random subset, isolating output collection from the precreation
finding. Existing fixed bug targets retain the strict protocol expectations.

Round 4 also found an extra-output manifestation of the mixed-field symlink
implementation: an ignored legacy file declaration emits a top-level link from
a requested directory Tree. `ignored_legacy_symlink` is the minimal bug-tagged
repro; findings/executor-outputs-ignored-legacy-symlink.md records two fresh
reproductions. The property oracle permits only that documented extra entry and
continues to verify the link inside the Tree. Modern-only compatibility link
fields are optional and checked against explicitly selected links and target types.
