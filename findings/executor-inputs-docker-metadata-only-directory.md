# Docker actions fail on a stored metadata-only empty input directory

Title: Metadata-only empty directory is reported missing on the Docker execution path
Components: app / executor
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Actions using optional metadata on empty directories fail before execution; stored content is not lost and no cross-tenant effect was observed.
Status: unverified
Repro test target: //tests/executor/inputs-docker:input_tree_limits_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/cbd39c1c-5137-4d82-ad32-ed072fde0998

## Expected behavior

A root containing one empty child Directory with NodeProperties must not report
that stored child as missing. The test reads its nonzero serialized blob back
from CAS before Execute. [REAPI GetTree](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L416)
enumerates stored Directory messages. This does not assert that optional mtime
or unix_mode must be applied: property support is server-defined.

## Actual behavior

The reduced `optional-node-properties` subtest returns FAILED_PRECONDITION (9)
wrapping GetTree NotFound, despite successful CAS upload/readback. The known
[bare-path finding](app-input-tree-metadata-only-directory.md) also reproduces
on the Docker execution path. Attribution remains app CAS traversal.

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
code:9 message:"download inputs: rpc error: code = NotFound desc = could not get input tree: Found empty directory for digest e7c2fbe24b99b655682700e641b5002568c7a8194b1273e4af7feb71da404a49."
```

## Notes

Released [fetchDirectory](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L795)
rejects decoded children with no files/directories/symlinks, ignoring metadata.
The canonical zero-byte empty Directory is special-cased; metadata changes its
digest and triggers the rejection. The paired nonempty metadata control and
ordinary empty-directory coverage exercise the same fixture. Workaround: omit
optional metadata on empty directories. The minimal repro has just one child
Directory; uploaded files used by the sibling hardlink subtest are unreachable
from this action root.

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
