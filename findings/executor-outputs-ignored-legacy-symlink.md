# Ignored legacy declaration adds an unrequested top-level symlink

Title: Ignored legacy declaration adds an unrequested top-level symlink
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Mixed-field clients receive an extra top-level output entry from an ignored declaration. This violates output selection and can cause clients to materialize or enumerate an unrequested output separately; the same link already belongs to the requested directory Tree, limiting impact. No additional data disclosure or cross-group impact is demonstrated.
Status: confirmed
Repro test target: //tests/executor/outputs:ignored_legacy_symlink
BuildBuddy invocation: https://app.buildbuddy.io/invocation/baf69cc7-f8a6-450a-938a-8e3200a5017d

## Expected behavior

[REAPI Command.output_paths](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843) supersedes output_files and output_directories: both legacy fields must be ignored. A requested directory returns its descendants inside its Tree. The optional legacy compatibility symlink fields may mirror explicitly requested modern symlinks; they do not authorize top-level entries for unrequested descendant paths.

With output_paths=[tree] and output_files=[tree/link], the command creates tree/link -> ../target and a regular target file. The result should contain one OutputDirectory for tree, whose Tree contains the link, and no top-level symlink entries.

## Actual behavior

The result contains the correct directory Tree and also OutputFileSymlink(path=tree/link, target=../target), even though tree/link is not an output_paths entry. The ignored legacy declaration affects output selection.

The seeded property test discovered this at seed 20261013, case 002 (dangling link in a selected directory). Shrinking removed the second subtree, other declarations, nested directories, and all random choices. The fixed repro uses a regular target to avoid legacy dangling-link type ambiguity. Its command is:

```sh
mkdir tree; printf data > target; ln -s ../target tree/link
```

The minimal fixed target reproduced in two independent fresh fixture executions:
[run 1](https://app.buildbuddy.io/invocation/baf69cc7-f8a6-450a-938a-8e3200a5017d),
[run 2](https://app.buildbuddy.io/invocation/2d73e8f7-3961-4d5a-a2a0-7d86cc92cf79).
Both pass status OK, exit-code, directory selection, CAS digest/readback, and Tree link-target assertions before failing on the extra top-level link. No auth, readiness, shell, or networking failure occurred. Initial property discovery: [invocation](https://app.buildbuddy.io/invocation/55b24252-01cb-4126-9f9a-fecbbfc6504b); that exploratory run also exposed an oracle assumption about permitted legacy compatibility fields, corrected separately before final coverage.

## Logs excerpt

```text
TestIgnoredLegacySymlinkDoesNotEscapeDirectoryTree:
status=<nil> result=output_file_symlinks:{path:"tree/link" target:"../target"}
  output_directories:{path:"tree" tree_digest:{...}}
Error: Should be empty, but was [path:"tree/link" target:"../target"]
Messages: output_files is ignored when output_paths is set; tree/link belongs only in the directory Tree
```

## Notes

[Released handleSymlink](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L400) selects modern behavior only when both legacy lists are empty. With mixed fields it checks legacy membership for every traversed symlink, including links inside selected directories. This is a distinct extra-output manifestation of the same branch implicated by [the existing missing-symlink finding](executor-outputs-output-paths-symlink-loss.md), not evidence of a separate underlying implementation defect. Mitigation: populate only output_paths.

Baseline harness.Start(App, Executor), empty input Directory, fixture-local self-auth/group/API key and Redis, bounded RPC context, workload-isolation-type=none, no image/config/flag overrides, DoNotCache=true and SkipCacheLookup=true. Containers start inside each test on bb remote runners; no shared component endpoint or dispatch credential is used. Harness artifacts preserve configs, container logs and inspect metadata. Independent verification is recorded below.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:ignored_legacy_symlink \
  --test_tag_filters=bug --nocache_test_results --test_output=all
```

The passing declaration_property oracle allows only this exact extra legacy-file-link case when the link is beneath a requested directory, and still checks its path/target and Tree representation. The fixed bug target keeps the strict REAPI expectation.


## Verification

Verdict: **confirmed**. Independently reviewed PR #108 at commit
`56f4beb08e8aa3477dc65a087ff4a62934ea6ac6` on 2026-10-13. The minimal test
correctly exercises output selection; no test or severity change is needed.

The [pinned REAPI Command specification](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L843-L871)
says only listed output paths are returned, selected directories are returned as
Trees, and legacy fields are ignored when `output_paths` is used. The
[ActionResult compatibility provision](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L1266-L1300)
allows legacy fields alongside modern symlink entries for requested symlinks;
it does not make an ignored legacy declaration an additional request. Here the
only selected path is the directory `tree`, so `tree/link` belongs in its Tree,
not in a separate top-level output entry. Mixed declarations are not prohibited
by the cited specification.

At [v2.314.0, NewDirHelper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L127-L152)
correctly selects only `output_paths`. However,
[handleSymlink](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L398-L463)
adds a traversed descendant to its Directory, then enters the legacy branch
because `OutputFiles` is nonempty and appends the link based on that ignored
list. [Workspace output upload](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go#L553-L564)
passes the original Command to this handler through UploadTree, explaining the
observed result. This is deterministic selection logic, not a timing race.

I checked the released source's documentation under `docs/`; no documented
exception to output-path precedence or intended descendant duplication was
found. The [mixed-field source test](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools_test.go#L211-L251)
expects a legacy symlink for a path explicitly listed in both fields; that case
does not authorize this unrequested descendant. The
[modern directory source test](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools_test.go#L444-L481)
expects its descendant symlink in the Tree without a top-level entry.

Two separate uncached runs of the unchanged bug target reproduced the identical
failure at `ignored_symlink_test.go:25`:

- [Run 1 Bazel invocation](https://app.buildbuddy.io/invocation/18dbafd3-e87e-4b1f-804d-454af80188a1), [runner invocation](https://app.buildbuddy.io/invocation/4d18821f-df24-45fb-95d3-fdc0cbfb5d44): failed after 33.31 seconds.
- [Run 2 Bazel invocation](https://app.buildbuddy.io/invocation/6e36c1a8-bc4f-474d-b40f-e5f12bb5a46b), [runner invocation](https://app.buildbuddy.io/invocation/37e24bf3-880d-4e13-b452-5499a969c1c0): failed after 31.11 seconds.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:ignored_legacy_symlink \
  --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

Both logs identify the exact app/executor digests listed above. Each fresh fixture
completed local auth/bootstrap and execution with status OK and exit code 0.
Directory selection, CAS digest/readback, and the Tree symlink name/target checks
all passed before the assertion rejected `output_file_symlinks:{path:"tree/link"
target:"../target"}`. The action has an empty input root, a bounded context,
`DoNotCache=true`, and `SkipCacheLookup=true`; no custom config or image override,
shared component endpoint, or dispatch API key enters the fixture. These results
rule out a cached response, dangling-target ambiguity, and harness/readiness or
shell failure as explanations.

Severity remains **low**: this is an extra output representation of a link already
present in the requested Tree, with no demonstrated additional content exposure
or execution failure. It shares the mixed-field handler cause described in the
existing missing-symlink report; it should not be counted as a separate root cause.
