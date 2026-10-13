# Local rigor audit (2026-10-13)

Audited origin/cand/sm1/cas-state at e06d5a207b5e269e65e821d14ab03d8165df047b.
Verdict: shallow relative to the every-surface / app-proxy agreement requirement.
Assertions are real, but original tests only read from the writer component.
The app and proxy use different instances and content. Every mutation rewrites
all modeled blobs, potentially repairing lost old state before preservation
assertions run.

Three independent perturbations, each restored before the next run:
- Post-write missing count 0 -> 1: failed both components as expected.
  https://app.buildbuddy.io/invocation/996904dc-c38f-4020-b4ba-c43e49a52c94
- Metadata logical size -> size+1: failed both components as expected.
  https://app.buildbuddy.io/invocation/a90dea66-7d42-49e2-8268-c1a82a9ff0bc
- Model directory child counts -> count+1: failed app size assertion as expected.
  https://app.buildbuddy.io/invocation/4eb8d1ec-e0fe-47fc-a1ec-ff9a9cae4c1e

Local added checks reuse each writer's same model and instance on the peer after
all six batches, and assert status and exact bytes for /file/download and
/file/view. Passed uncached on pinned v2.314.0 app/proxy images:
https://app.buildbuddy.io/invocation/efe0a7fb-6ae6-4b48-b863-6a05eb8ab9cf
No new divergence observed. These additional reads are sequential; they prove
agreement at the time queried, not independent cache residency or invalidation.

Original checks assert existence, exact batch/read/CLI bytes, tree directory
sets, logical metadata size, nonzero storage size/modification time, UI metadata
sizes and directory size/count maps, and public file bytes/ranges. App UI and
public APIs are asserted over HTTP and gRPC. Proxy legitimately lacks these
app-only services. No test skips, swallowed RPC errors, or polling loops found.
The numeric JSON parser ignores parse errors; tree set comparison discards
multiplicity; size-map construction discards duplicate keys; metadata access
and modification transitions are not checked. The hash-only size keys follow
the implementation despite the released proto's hash/size comment.

Further missing coverage: compressed batch/ByteStream reads, ByteStream ranges,
tree pagination, and bb download --output_directory filesystem contents.
ByteStream QueryWriteStatus is unimplemented in released source, so it is not
an additional supported CAS-state oracle. ByteStream.Write/DeleteFile mutations
are outside the branch's stated BatchUpdateBlobs scope.

Nothing pushed and no PR opened. No shared harness changes required.
