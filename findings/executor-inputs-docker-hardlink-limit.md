# Docker input materialization hits the repeated-content hardlink limit

Title: Repeated-digest input tree fails at the hardlink limit under Docker isolation
Components: executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: A valid approximately 960 KiB tree fails before execution when many paths share content; no crash or cross-tenant effect was observed.
Status: unverified
Repro test target: //tests/executor/inputs-docker:input_tree_limits_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/cbd39c1c-5137-4d82-ad32-ed072fde0998

## Expected behavior

The valid canonical Merkle DAG (256 directories each referencing the same
256-file directory, all files referencing a 15-byte blob) should materialize.
The [REAPI Directory contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L978)
permits repeated content digests. This is a filesystem-dependent reliability
finding, not a claim that REAPI promises unlimited resources.

## Actual behavior

The `65536-files-256-directories` subtest returns UNKNOWN (2), `too many links`,
at `d253/f231`. The known [bare-isolation finding](executor-input-tree-hardlink-limit.md)
also reproduces under Docker. Uploaded nodes are sorted/unique and the same
fixture can schedule valid small Docker input trees.

Two independent uncached fixture runs reproduce this under Docker isolation:

1. https://app.buildbuddy.io/invocation/cbd39c1c-5137-4d82-ad32-ed072fde0998
2. https://app.buildbuddy.io/invocation/1cd129b6-7fee-4a2e-bd75-6122ee2dcbe8

Both used the pinned images above, disposable fixture credentials and local CAS /
execution endpoints. Neither execution reaches the command. The initial run
https://app.buildbuddy.io/invocation/3b1df47d-18fa-4bc8-b43d-d60e6e530c05
failed during app/Redis readiness with bridge forwarding disabled; it is excluded
from reproduction evidence. Enabling normal Docker iptables rules and using the
harness startup lock resolved that configuration error.

The [passing Docker regression suite](https://app.buildbuddy.io/invocation/def534ad-5a64-4275-b5a0-b988c69125c1)
validates ordinary empty directories, nonempty metadata controls, repeated
digests, and seeded cold/warm exact-tree checks in the corrected fixture.

## Logs excerpt

```text
code:2 message:"download inputs: could not fetch inputs: link .../d000/f000 .../d253/f231: too many links"
```

## Notes

The released [dirtools copyFile](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L692)
uses [FastCopy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/fastcopy/fastcopy_linux.go),
which propagates EMLINK rather than falling back to a new inode. The host-backed
workspace reaches the same finite link limit as the bare fixture. No inode
identity assertion is imposed on the action; hardlinking is an internal
optimization. Mitigation: reduce per-digest multiplicity. Suggested remedy:
copy onto a fresh inode on EMLINK.

Actions request `workload-isolation-type=docker`, Linux/amd64 and
`container-image=docker://mirror.gcr.io/library/alpine@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507`.
The fixture owns its daemon/socket/network, with aligned host workspace bind
mounts. There is no image override, component source build, recycled runner or
shared deployment RPC. Input materialization happens before action-container
startup; this demonstrates the defect on the Docker execution path, not a
separate Docker-container implementation defect. Full logs/configs/image inspect
metadata are in undeclared outputs. Status remains unverified for maintainer
review, as requested by this area's task, despite two reproductions.

Reproduce with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/inputs-docker:input_tree_limits_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

Select one case with `--test_arg=-test.run=TestInputTreeLimits/<subtest>`.
Findings remain in this repository; no upstream report was filed.
