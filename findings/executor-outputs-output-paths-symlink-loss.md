# Legacy fields suppress symlinks requested through output_paths

Title: Legacy fields suppress symlinks requested through output_paths
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Successful actions silently lose a requested output when clients populate both modern and legacy output fields. This affects result correctness and client output materialization; it requires mixed-field commands.
Status: confirmed
Repro test target: //tests/executor/outputs:output_paths_precedence
BuildBuddy invocation: https://app.buildbuddy.io/invocation/f9c1b40e-14d7-4022-bb59-fb39955385d8

## Expected behavior

[REAPI Command.output_paths](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) says output_files and output_directories are ignored when output_paths is used. ActionResult.output_symlinks must contain each requested output path that exists and is a symlink.

## Actual behavior

The command creates target containing data and link -> target, requests output_paths=[link], and also sets output_files=[ignored]. Execution succeeds but returns no output files, directories, or symlinks. The requested link is lost.

The same TestOutputPathsOverrideLegacySymlinks assertion failed in both linked invocations. The fixture uses baseline bare execution, an empty input root, and no custom flags. The passing regression symlink case returns file, directory, and dangling links with output_paths alone, and the regular-file mixed-field regression passes. This isolates the failure to mixed-field symlink handling.

Reproductions: [run 1](https://app.buildbuddy.io/invocation/f9c1b40e-14d7-4022-bb59-fb39955385d8), [run 2](https://app.buildbuddy.io/invocation/3634697f-9f13-4fd6-b4c6-3b3dbe81faed).

## Logs excerpt

Test RPC/assertion excerpts (unrelated execution metadata omitted):

```text
--- FAIL: TestOutputPathsOverrideLegacySymlinks
status=<nil> result=stdout_digest:{...} stderr_digest:{...} execution_metadata:{...}
Error: "[]" should have 1 item(s), but has 0
Messages: output_paths supersedes output_files; requested symlink must be returned
```

## Notes

[handleSymlink](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go) enters its modern branch only when both legacy fields are empty. With mixed fields, it instead checks the symlink against ignored legacy lists. Mitigation: send only output_paths. Run the minimal case with --test_filter=TestOutputPathsOverrideLegacySymlinks.

Both runs use the pinned images above with harness.Start(App, Executor), default SQLite/local CAS and test-local Redis, and workload-isolation-type=none. Actions set DoNotCache=true and SkipCacheLookup=true. No version overrides or shared component endpoints are used. Harness cleanup preserves container configs/logs/inspect data in undeclared outputs. Reports stay in this repository.

```sh
bb remote test //tests/executor/outputs:output_paths_precedence --test_tag_filters=bug --nocache_test_results --test_output=all
```

Passing coverage: [nobug suite](https://app.buildbuddy.io/invocation/3850b6f5-57a5-4b11-9bd5-7c4e657a7451).

## Verification

Verdict: **confirmed**. Retain **medium** severity: a successful action silently
loses a requested artifact. Mixed fields are required and sending only modern
fields avoids the issue, limiting exposure without making the request invalid.

`TestOutputPathsOverrideLegacySymlinks` reproduces in both independent runs:
status 0, exit code 0, and no output symlink or other requested artifact.
The shell creates a valid relative file symlink in an empty input workspace;
there is no dangling-target, absolute-link policy, or output ordering issue.
The ignored legacy path need not exist. Output omission follows a deterministic
branch in the release implementation, rather than timing or CAS readback.

[Command.output_paths](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843-L871)
explicitly supersedes and ignores both legacy fields. The
[output_symlinks contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1286-L1300)
has potentially confusing wording (“and not” the legacy fields), but it must be
read with that precedence rule and the requirement to return each requested
existing symlink. It does not authorize dropping a requested path merely because
an ignored field was populated. Even a different interpretation of which symlink
result field to populate would not explain losing the link from every field.

[handleSymlink](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L418-L463)
requires both legacy fields to be empty before entering the modern branch.
Otherwise it checks membership in the ignored legacy lists. Here `link` is not
in `output_files=[ignored]`, so no result entry is emitted. In contrast,
[NewDirHelper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L127-L153)
correctly selects `output_paths`. No documented intended exception was found.


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

All five releases failed at the finding's intended assertion: The action
returned OK but OutputSymlinks was empty for the requested link.
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
