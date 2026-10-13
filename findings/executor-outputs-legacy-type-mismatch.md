# Executor accepts legacy output type mismatches

Title: Executor accepts legacy output type mismatches
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Legacy REAPI clients receive successful results with incompatible output types instead of a protocol error, potentially hiding broken build rules or failing output materialization. No cross-tenant boundary is implicated.
Status: confirmed
Repro test target: //tests/executor/outputs:legacy_type_mismatch
BuildBuddy invocation: https://app.buildbuddy.io/invocation/f9c1b40e-14d7-4022-bb59-fb39955385d8

## Expected behavior

[REAPI ActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) requires FAILED_PRECONDITION (9) when an output_files path is a directory, or a legacy output symlink points to the wrong type. The corresponding output_directories contract likewise requires directory outputs. These commands use only legacy fields, so modern type inference does not apply.

## Actual behavior

All four cases return status OK (0), exit code 0: directory declared as file is returned in output_directories; file declared as directory is returned in output_files; a directory symlink declared as file is returned in output_file_symlinks; a file symlink declared as directory is returned in output_directory_symlinks.

Each case reproduced in both linked invocations on independent fixture-local containers. The directory-declared cases remove the executor-created empty output directory before creating the actual file/symlink; the command succeeds, ruling out a shell setup failure. No output_paths field is used. The assertion compares numeric status codes, not protobuf object internals.

Reproductions: [run 1](https://app.buildbuddy.io/invocation/f9c1b40e-14d7-4022-bb59-fb39955385d8), [run 2](https://app.buildbuddy.io/invocation/3634697f-9f13-4fd6-b4c6-3b3dbe81faed).

## Logs excerpt

Test RPC/assertion excerpts (unrelated execution metadata omitted):

```text
TestLegacyOutputTypeMismatch/directory-as-file:
status=<nil> result=output_directories:{path:"wrong" tree_digest:{size_bytes:2 ...}}
expected: 9
actual:   0
TestLegacyOutputTypeMismatch/file-as-directory:
status=<nil> result=output_files:{path:"wrong" digest:{size_bytes:4 ...}}
TestLegacyOutputTypeMismatch/directory-symlink-as-file:
status=<nil> result=output_file_symlinks:{path:"wrong" target:"target"}
TestLegacyOutputTypeMismatch/file-symlink-as-directory:
status=<nil> result=output_directory_symlinks:{path:"wrong" target:"target"}
```

## Notes

[UploadTree and handleSymlink](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go) select paths by membership but do not enforce the legacy declared output type. The four small shell commands are separate subtests in types_test.go. Modern output_paths callers can legitimately infer file/directory types, but legacy callers cannot.

Both runs use the pinned images above with harness.Start(App, Executor), default SQLite/local CAS and test-local Redis, and workload-isolation-type=none. Actions set DoNotCache=true and SkipCacheLookup=true. No version overrides or shared component endpoints are used. Harness cleanup preserves container configs/logs/inspect data in undeclared outputs. Reports stay in this repository.

```sh
bb remote test //tests/executor/outputs:legacy_type_mismatch --test_tag_filters=bug --nocache_test_results --test_output=all
```

Passing coverage: [nobug suite](https://app.buildbuddy.io/invocation/3850b6f5-57a5-4b11-9bd5-7c4e657a7451).

## Verification

Verdict: **confirmed**. Retain **medium** severity: legacy clients receive
successful but incorrectly typed results, which can break materialization or
hide erroneous build rules. This is protocol correctness, not a tenant boundary
or a defect in modern output type inference.

All four subtests reproduce with status 0 and exit code 0 in both independent
runs, rather than FAILED_PRECONDITION (9). The directory-declared scripts remove
the precreated path before writing, so the legacy directory workaround cannot
explain the incorrect results. The comparison uses numeric status values;
protobuf representation differences are irrelevant.

The [REAPI ActionResult contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1253-L1384)
explicitly requires FAILED_PRECONDITION for directory-as-file and both symlink
target type mismatches. The ordinary file-as-directory paragraph at lines
1361–1363 has a wording error (it mentions `output_files`); the surrounding
`output_directories` contract requires directory results. That ambiguity does
not undermine the other three explicit violations or justify returning a regular
file for a legacy directory declaration. All commands have empty `output_paths`.

In [NewDirHelper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L127-L153),
legacy declarations become an untyped path set.
[UploadTree](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L467-L545)
selects result fields from actual filesystem type without legacy type validation;
[handleSymlink](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L400-L463)
copies links to legacy result fields based on declaration membership without
checking their targets. This explains the deterministic failures. Unlike the
legacy precreation workaround, no intentional type-mismatch compatibility
exception was found in these code paths or the checked docs.


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

All five releases failed at the finding's intended assertion: All four legacy
type-mismatch cases returned OK (0), rather than FAILED_PRECONDITION (9).
The oldest release was reproduced in two independent fresh fixtures.

**Earliest tested reproducing release: v2.258.0 (2026-04-09).** The defect
was present at or before that release; its actual introduction is unknown.
No earlier release or known-good boundary was tested, so this is an upper
bound on introduction, not evidence that v2.258.0 introduced it.

No release-specific configuration changes were needed for these five releases.

Command template, substituting the matching digests from the table below:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:legacy_type_mismatch \
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
