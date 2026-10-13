# Ignored legacy declaration adds an unrequested top-level symlink

Title: Ignored legacy declaration adds an unrequested top-level symlink
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Mixed-field clients receive an extra top-level output entry from an ignored declaration. This violates output selection and can cause clients to materialize or enumerate an unrequested output separately; the same link already belongs to the requested directory Tree, limiting impact. No additional data disclosure or cross-group impact is demonstrated.
Status: unverified
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

Baseline harness.Start(App, Executor), empty input Directory, fixture-local self-auth/group/API key and Redis, bounded RPC context, workload-isolation-type=none, no image/config/flag overrides, DoNotCache=true and SkipCacheLookup=true. Containers start inside each test on bb remote runners; no shared component endpoint or dispatch credential is used. Harness artifacts preserve configs, container logs and inspect metadata. Status remains unverified for maintainer review as requested.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs:ignored_legacy_symlink \
  --test_tag_filters=bug --nocache_test_results --test_output=all
```

The passing declaration_property oracle allows only this exact extra legacy-file-link case when the link is beneath a requested directory, and still checks its path/target and Tree representation. The fixed bug target keeps the strict REAPI expectation.
