# Docker isolation: overlapping directories produce incomplete parent Tree

Title: Docker isolation: overlapping directories produce incomplete parent Tree
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; workload Alpine 3.24.2 mirror.gcr.io/library/alpine@sha256:d56c381f961d307a21b3ca004cf1e3910f106644aefb1f43e654c8a56c4fd395
Severity: medium
Severity justification: A successful parent Tree lacks the Directory messages needed to materialize descendants. Clients restoring that Tree can fail; overlapping declarations are required.
Status: unverified
Repro test target: //tests/executor/outputs-docker:overlapping_directories
BuildBuddy invocation: https://app.buildbuddy.io/invocation/d6f2abde-f559-4a6b-8ca8-71d18c9960f9

## Expected behavior

Each returned Tree must contain its root and all descendant Directory messages. A separately declared nested output directory must still be included in its parent Tree.

[REAPI Command, ActionResult and Tree contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

## Actual behavior

The command creates tree/nested/file and declares output_paths=[tree,tree/nested]. Both OutputDirectories are returned, but the parent Tree has no Children even though its root references nested.

The corresponding bare-isolation finding **also reproduces under Docker** in
[run 1](https://app.buildbuddy.io/invocation/d6f2abde-f559-4a6b-8ca8-71d18c9960f9) and [run 2](https://app.buildbuddy.io/invocation/1c11d133-6aef-412d-b0a1-59b5404def25), on independent fixture-local containers.
Both runs reach successful action execution (status OK, exit code 0), then fail
the output assertion above. Initial attempts with iptables disabled failed Redis
readiness before actions ran; those are configuration failures and are excluded.
The corrected private daemon enables Docker network rules and holds the harness
startup lock for the fixture lifetime. No image overrides are used. Passing
modern/legacy file, symlink, nested-Tree and CAS-integrity coverage rules out a
mount, transport, or generic output-upload setup error.

## Logs excerpt

```text
TestOverlappingDirectories
status=<nil> exit_code=0; output_directories: tree, tree/nested
Error: "[]" should have 1 item(s), but has 0
```

## Notes

UploadTree assigns a visited directory either as a Tree root or as another Tree child, not both. The resulting parent Tree is incomplete. Single-output nested Trees pass under Docker.

Bare report: `findings/executor-outputs-overlapping-directory-tree.md` on the source area branches
(PRs #23/#60/#74). Released implementation:
[dirtools.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go)
and [workspace.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/workspace/workspace.go).

Actions use workload-isolation-type=docker, the pinned mirror workload image,
an empty input root, DoNotCache=true, SkipCacheLookup=true and bounded contexts.
All RPCs address the harness-started local app. The private dockerd socket and
workspace are mounted into the executor, with identical host/container workspace
paths. Container configs/logs/inspect metadata and daemon logs are preserved as
undeclared test outputs. Status remains unverified for maintainer review.

Passing suite: [regression, concurrent/unstable uploads, and seeded bare/Docker
comparison](https://app.buildbuddy.io/invocation/b178eaf6-d3a8-4d00-9dab-9fe5703df28d).

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/outputs-docker:overlapping_directories --test_tag_filters=bug --nocache_test_results --test_output=all
```
