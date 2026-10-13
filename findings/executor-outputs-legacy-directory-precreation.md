# Ignored legacy output directories alter modern action execution

Title: Ignored legacy output directories alter modern action execution
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Mixed-field commands can take the wrong branch because ignored legacy directories are created before execution. Retain low severity: the effect is confined to the action workspace, requires mixed fields, and is avoided by sending output_paths alone. Legacy-only precreation is an intentional compatibility workaround, not an independent unintended defect.
Status: confirmed
Repro test target: //tests/executor/outputs:output_paths_precedence (TestOutputPathsIgnoreLegacyDirectoryCreation); legacy-only behavior probe: //tests/executor/outputs:missing_legacy_directory
BuildBuddy invocation: https://app.buildbuddy.io/invocation/f9c1b40e-14d7-4022-bb59-fb39955385d8

## Expected behavior

[REAPI Command.output_directories](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) says only parent directories are created before execution, not output directories themselves. BuildBuddy intentionally deviates for legacy-only commands to support older Bazel rules. The actionable defect here is that when output_paths is set, legacy output_directories must be ignored entirely, including during workspace setup.

## Actual behavior

Historical legacy-only probe: a no-op shell command with output_directories=[missing] returns an empty Tree for missing instead of omitting it. This reproduced in both linked invocations and is explained by the intentional compatibility workaround.

The confirmed mixed-field defect (TestOutputPathsIgnoreLegacyDirectoryCreation in //tests/executor/outputs:output_paths_precedence) sets output_paths=[out], output_directories=[ignored] and writes unexpected if ignored already exists, expected otherwise. Both runs returned the SHA256 digest bcf7690127d5b0c019c22e0472b2bb4e8d98784c6e7414f29436513a1c60853f of unexpected. The second run checks readback bytes with a semantic protobuf digest comparison; the first run stopped at an unrelated structural protobuf comparison after logging the same digest.

Inputs are an empty Directory; fixture baseline configuration has no output creation customizations. Modern missing-directory regression coverage passes. This is not a CAS readback or harness filesystem effect: the shell action executes inside the released executor and the release source explicitly creates these directories.

Reproductions: [run 1](https://app.buildbuddy.io/invocation/f9c1b40e-14d7-4022-bb59-fb39955385d8), [run 2](https://app.buildbuddy.io/invocation/3634697f-9f13-4fd6-b4c6-3b3dbe81faed).

## Logs excerpt

Test RPC/assertion excerpts (unrelated execution metadata omitted):

```text
--- FAIL: TestMissingLegacyDirectoryIsOmitted
status=<nil> result=output_directories:{path:"missing" tree_digest:{hash:"102b51b9765a56a3e899f7cf0ee38e5251f9c503b357b330a49183eb7b155604" size_bytes:2}}
Error: Should be empty, but was [path:"missing" tree_digest:{...}]
TestOutputPathsIgnoreLegacyDirectoryCreation:
output_files:{path:"out" digest:{hash:"bcf7690127d5b0c019c22e0472b2bb4e8d98784c6e7414f29436513a1c60853f" size_bytes:10}}
```

## Notes

[NewDirHelper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go) documents this as a workaround for old Bazel rules, but unconditionally appends the legacy output directories to dirsToCreate, including when output_paths supersedes them. Maintainers should assess compatibility requirements against REAPI conformance. Mitigation: use output_paths without legacy fields. The primary no-op repro demonstrates the intentional legacy workaround; the mixed-field case is the confirmed defect. The no-op assertion should become a compatibility behavior test, rather than a bug-tagged conformance failure.

Both runs use the pinned images above with harness.Start(App, Executor), default SQLite/local CAS and test-local Redis, and workload-isolation-type=none. Actions set DoNotCache=true and SkipCacheLookup=true. No version overrides or shared component endpoints are used. Harness cleanup preserves container configs/logs/inspect data in undeclared outputs. Reports stay in this repository.

```sh
bb remote test //tests/executor/outputs:output_paths_precedence --test_filter=TestOutputPathsIgnoreLegacyDirectoryCreation --test_tag_filters=bug --nocache_test_results --test_output=all
```

Passing coverage: [nobug suite](https://app.buildbuddy.io/invocation/3850b6f5-57a5-4b11-9bd5-7c4e657a7451).

## Verification

Verdict: **needs-work** on the original report; **confirmed** for the narrowed
mixed-field defect. `Status: confirmed` records that actual defect using the
required confirmed/rejected status vocabulary. Retain **low** severity for this
workspace-local mixed-field compatibility issue, with the justification revised
above. The report title and primary repro pointer now identify the narrower bug.

Both independent runs reproduce the legacy-only no-op assertion failure and the
mixed-field assertion failure. `TestMissingLegacyDirectoryIsOmitted` receives an
empty output Tree; `TestOutputPathsIgnoreLegacyDirectoryCreation` returns the
bytes `unexpected`, not `expected`, after a successful action. Readback checks
content against the returned digest, so this is not a structural protobuf
comparison or CAS error. Fresh volumes and an empty input root rule out leftover
directories. The filesystem branch executes inside the released executor.

There is a mundane explanation for the primary legacy-only case:
[NewDirHelper lines 156–169](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L156-L169)
explicitly documents a workaround for older Bazel rules, with a reference to
[Bazel PR #15366](https://github.com/bazelbuild/bazel/pull/15366). It intentionally
creates legacy output directories. This does deviate from the
[REAPI parent-only setup contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L811-L871),
but the empty Tree is a consequence of that intentional workspace setup, not an
independent accidental upload failure. The original report should not present
this legacy-only behavior as an unexplained product defect.

The modern case remains a real defect: the same REAPI contract says legacy fields
are ignored when `output_paths` is supplied. The workaround nevertheless loops
over `cmd.GetOutputDirectories()` unconditionally, after correctly selecting
modern paths earlier in the function. It therefore creates `ignored` even though
it is neither an input nor a requested modern output. Compatibility with older
clients does not require altering a modern client's workspace this way.
[Workspace.CreateOutputDirs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go#L254-L264)
delegates to this helper; the harness has no output-directory setup customization.

Required test follow-up: remove the legacy-only no-op assertion from bug coverage
or convert it to a compatibility behavior assertion; use
`TestOutputPathsIgnoreLegacyDirectoryCreation` as the defect reproducer. No test
was edited in this review because its authorized scope is the three finding
files. The earlier logs remain historical evidence of both behaviors.


### Independent reproduction setup

Reviewed PR #23 at `origin/area/executor-outputs`, and reran its unchanged
bug-tagged tests twice on hosted runners. The logs identify the exact app and
executor digests listed above. Each test starts isolated fixture-local containers
with fresh `/data` volumes; the empty input root, shell commands, bounded RPCs,
`DoNotCache=true`, and `SkipCacheLookup=true` rule out stale action results or
input directories. No image overrides or shared component endpoints were used.
`--nocache_test_results` forces execution; the final empty tag filter overrides
`nobug` and both invocations execute all three targets.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test \
  //tests/executor/outputs:legacy_type_mismatch \
  //tests/executor/outputs:output_paths_precedence \
  //tests/executor/outputs:missing_legacy_directory \
  --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Independent Bazel invocations: [run 1](https://app.buildbuddy.io/invocation/f64c296a-d580-4c5d-a2f1-194fec893845),
[run 2](https://app.buildbuddy.io/invocation/740a7a17-3382-4cd8-a364-b877711c594a).
Outer runner invocations: [run 1](https://app.buildbuddy.io/invocation/eb1b209e-705e-483e-8981-6f408ec86851),
[run 2](https://app.buildbuddy.io/invocation/b315c977-3fb9-45db-a55b-6070462a7afc).

Source inspection used release tag `v2.314.0` (commit
`974929724a4c75f824ab18fbcb560e48bbf2803a`). Also checked the release's
[remote execution docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/remote-build-execution.md),
[RBE platform docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/rbe-platforms.md),
and [troubleshooting docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/troubleshooting-rbe.md).
They provide no output-contract exception relevant to this defect; the legacy
precreation workaround is documented in source, as distinguished in the verification reasoning.

## Version history

Tested on 2026-10-13 using the unchanged bug-tagged reproducer from
`origin/area/executor-outputs-2` at `f953aa418f749a1b102df6c1b61f85a458fabeb4`.
App, Redis and executor were started inside each test on hosted `bb remote`
runners, with fixture-local endpoints and disposable local credentials.
No BuildBuddy component was built from source.

| Enterprise release (release date, UTC) | Result | BuildBuddy invocation |
| --- | --- | --- |
| v2.314.0 (2026-10-08) | Reproduces | [run](https://app.buildbuddy.io/invocation/2e96ab0a-8cb9-4f4d-919b-fd6e34541405) |
| v2.313.0 (2026-10-06) | Reproduces | [run](https://app.buildbuddy.io/invocation/71022919-3179-42ac-8801-1bdb50ee0ebb) |
| v2.290.0 (2026-07-29) | Reproduces | [run](https://app.buildbuddy.io/invocation/aac11647-ec5c-43ca-8494-49ee4d857081) |
| v2.275.0 (2026-06-04) | Reproduces | [run](https://app.buildbuddy.io/invocation/2b8a9774-b8ad-4198-862b-700bd17c5072) |
| v2.258.0 (2026-04-09) | Reproduces | [run](https://app.buildbuddy.io/invocation/23c416f3-5e4f-493c-be8c-941325281908); [independent repeat](https://app.buildbuddy.io/invocation/4702a650-1fe0-4095-86a1-68796c425ba7) |

All five releases failed at the finding's intended assertion: The command
observed the ignored legacy directory and wrote "unexpected" instead of
"expected".
The oldest release was reproduced in two independent fresh fixtures.

**Earliest tested reproducing release: v2.258.0 (2026-04-09).** The defect
was present at or before that release; its actual introduction is unknown.
No earlier release or known-good boundary was tested, so this is an upper
bound on introduction, not evidence that v2.258.0 introduced it.

No release-specific configuration changes were needed for these five releases.

Command template, substituting the matching digests from the table below:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:output_paths_precedence \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image=gcr.io/flame-public/buildbuddy-app-enterprise@<app-digest> \
  --test_arg=-harness.executor_image=gcr.io/flame-public/buildbuddy-executor-enterprise@<executor-digest>
```

The invocation batches the five output bug targets; `output_paths_precedence`
contains separate symlink-loss and directory-precreation tests. The result
above was checked against this finding's individual Go test, not just the
aggregate Bazel failure.

### Exact release pins

Both components were overridden together. These linux/amd64 manifest digests
were resolved from the GCR registry API, and the fixture logs identify the
requested digest references. App repository:
`gcr.io/flame-public/buildbuddy-app-enterprise`; executor repository:
`gcr.io/flame-public/buildbuddy-executor-enterprise`.

| Enterprise release | App digest | Executor digest |
| --- | --- | --- |
| v2.314.0 | `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` | `sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8` |
| v2.313.0 | `sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` | `sha256:8f7c2662f5fcd0ab888113f6d36d31cde8b8ea254434a8177cf6996b01155afe` |
| v2.290.0 | `sha256:f3b285107f28da495e9496dd848857ce0cf6cddbf3fac8743b259c2a7a8678b6` | `sha256:2c65048231e21ecf0c60623bc5f8ccaf494e82828077c5064aa994909e15607a` |
| v2.275.0 | `sha256:75ffd43cf6dc14ccf5a652b760d7e4d7390dfa7870524c57e7f354f271e96db1` | `sha256:d2dbe25032467449c80e9eb18002d56b57bcf5247cba267ab886022415b30956` |
| v2.258.0 | `sha256:f7b499d817581e58c1fe826c6441a7773519488d4500dc6ffafad387d2c990c4` | `sha256:500703561ba144c64b12a36f0d3350422f09efe693b8e24592414958ea6753cd` |
