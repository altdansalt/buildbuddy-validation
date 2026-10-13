# Docker isolation: explicitly declared nested file omitted

Title: Docker isolation: explicitly declared nested file omitted
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; workload Alpine 3.24.2 mirror.gcr.io/library/alpine@sha256:d56c381f961d307a21b3ca004cf1e3910f106644aefb1f43e654c8a56c4fd395
Severity: low
Severity justification: Clients enumerating an explicitly requested file through OutputFiles cannot find it. The bytes remain available inside the directory Tree, limiting the impact.
Status: unverified
Repro test target: //tests/executor/outputs-docker:overlapping_file
BuildBuddy invocation: https://app.buildbuddy.io/invocation/d1eb4d81-c8c8-45c7-8dd9-74d5e386c813

## Expected behavior

Command.output_paths says a resulting file is returned in an OutputFile field.
An enclosing directory declaration does not remove an explicitly listed file.

[REAPI Command and ActionResult](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

## Actual behavior

The bare finding also reproduces under Docker. The reduced command is
`mkdir -p tree; printf inner > tree/file` with sorted output_paths=[tree,tree/file].
The action returns status OK, exit code 0, one OutputDirectory, and no OutputFiles.

This output assertion fails in both [run 1](https://app.buildbuddy.io/invocation/d1eb4d81-c8c8-45c7-8dd9-74d5e386c813)
and [run 2](https://app.buildbuddy.io/invocation/dbf60ba2-6805-4d6f-b644-b90385e63425)
on independent fixture-local containers. Both use the exact pinned images above.
Earlier reduced attempts used `mkdir tree`, which failed because REAPI creates
parents of declared output files. Those shell failures are excluded; `mkdir -p`
fixes the reproducer setup. Initial private-daemon attempts with iptables disabled
failed Redis readiness before execution and are also excluded. Corrected fixtures
enable Docker network rules and hold the harness startup lock for their lifetime.
Passing modern files and nested Trees rule out a generic mount/upload failure.

## Logs excerpt

```text
TestOverlappingDirectoryOutputs
status=<nil> result=output_directories:{path:"tree" tree_digest:{...}}
exit_code=0; output_files=[]
Error: "[]" should have 1 item(s), but has 0
Messages: explicitly declared file must be returned even beneath an output directory
```

## Notes

Bare report: `findings/executor-outputs-overlapping-file.md` in PR #60.
[Released UploadTree.visitFile](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go)
only adds an OutputFile when FindParentOutputPath finds no enclosing output path.
Mitigation: obtain the file from the directory Tree or avoid redundant declarations.

The fixture owns its dockerd, app, Redis and executor. The private daemon socket
and identical host/container workspace path are mounted into the executor.
Actions use workload-isolation-type=docker, the pinned mirror workload image,
an empty input root, DoNotCache=true, SkipCacheLookup=true and bounded contexts.
All RPCs address the fixture-local app. Configs/logs/inspect metadata and daemon
logs are preserved as undeclared test outputs. Status remains unverified for
maintainer review; no upstream report is filed.

Passing coverage: [regression, concurrent/unstable uploads, and seeded bare/Docker
comparison](https://app.buildbuddy.io/invocation/b178eaf6-d3a8-4d00-9dab-9fe5703df28d).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker:overlapping_file --test_tag_filters=bug --nocache_test_results --test_output=all
```
