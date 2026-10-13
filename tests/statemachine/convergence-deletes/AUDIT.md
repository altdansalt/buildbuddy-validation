# Local conformance audit

Verdict: shallow, not hollow. The original suite has real assertions but does not
cover every read surface or all mutable fields. Origin was never modified.

## Actual assertions after modeled mutations

- BatchUpdateBlobs / ByteStream.Write / CAS DeleteFile: app and proxy
  FindMissingBlobs presence (only boolean missing count, not returned digest),
  BatchReadBlobs status/bytes and ByteStream.Read status/bytes. App GetFile
  streaming gRPC and REST assert bytes; GetFileRange gRPC and REST assert a
  whole-file range, not a partial range. App /file/download and /file/view
  assert HTTP status and bytes. bb download asserts bytes on app/proxy only
  for the empty namespace, conditional on finding bb in PATH.
- UpdateActionResult / AC DeleteFile: app/proxy GetActionResult compare only
  stdout_raw and exit_code; app public GetFile gRPC/REST and UI download/view
  parse ActionResult but compare only stdout_raw. No changed exit codes,
  output files/directories, stderr, digest-backed stdout, or inline-read
  options are exercised. Server-generated metadata is deliberately excluded.
- CAS and AC metadata: BuildBuddyService/GetCacheMetadata gRPC and web JSON
  protolet, and cache.service.Cache/GetMetadata app/proxy assert only
  success/NotFound. Responses are discarded or decoded into Empty. No sizes,
  timestamps, or metadata contents are validated. Proxy AC original-key
  metadata is always expected absent because of request-key salting.
- Command/Directory/Action setup uploads and BES mutations are not followed
  individually by cross-surface checks.

## Gaps and pass mechanisms

GetTree (REAPI and frontend BuildBuddyService) and frontend
GetTreeDirectorySizes exist but are never tested. Neither are ByteStream
QueryWriteStatus, partial ranges, named-instance CLI downloads, or directory
CLI extraction. QueryWriteStatus is explicitly Unimplemented on this release,
so it needs a contract assertion rather than an assumed successful response.
GetFileRange is CAS-only, so AC range reads are not a missing supported surface.
Distributed-peer deletes require a different fixture and are disclosed as absent.

No unconditional test skip or vacuous early return was found. CLI absence is
logged and accepted. Ten-second AC polling can swallow transient errors but
cannot accept a permanently wrong stdout/exit code. REST missing-range checks
accept any non-200 status; missing CLI downloads accept any command error.
Warmed proxy CAS retention is explicitly allowed; app/proxy CAS byte visibility
therefore need not eventually converge in this suite. This is consistent with
its documented independent immutable-cache semantics, but narrower than the
requested eventual-agreement criterion.

## Assertion perturbations

Three independent subtests ran the original sequence with one wrong expectation
apiece: BatchRead response count 2 instead of 1; backing CAS present after
DeleteFile; and reference AC stdout set to "wrong reference revision" after
UpdateActionResult. All three failed at the intended assertions (the AC check
failed at its ten-second deadline).

https://app.buildbuddy.io/invocation/be72239b-b01e-4bc4-962d-26b4b93867e7

## Missing-surface probe

TestAuditDeletedTreeChild uploads a valid five-directory chain and warms GetTree,
then deletes its leaf directory using app DeleteFile. Direct app ByteStream.Read
returns NotFound and FindMissingBlobs reports the child missing. Cached app and
proxy GetTree nevertheless return all five directories, including the deleted
leaf. The frontend GetTree gRPC adapter also completes successfully. Two fresh
fixtures reproduced this in the first probe invocation:

https://app.buildbuddy.io/invocation/d4efbe9a-5c17-4b41-a6c7-bac2a1b7b331

The first probe asserted NotFound for a missing child; that expectation was too
strict because REAPI permits partial trees. The retained probe instead checks
that the deleted directory is absent from the returned tree. It repeats cached
cases twice, uploads all referenced file content, and adds a cache-disabled
control. The original sequence is also rerun unchanged in the final invocation.

Tree caching is enabled by default. The probe changes only its write probability
from 0.01 to 1 to remove sampling variability, plus the same delete API gate as
the original suite. The control disables tree caching. This is an observed
cross-surface divergence, not a claim of eventual nonconvergence over every
possible eviction interval or a separately filed upstream product defect.

Exact enterprise-v2.314.0 pinned images:

- app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
- proxy: gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906

Relevant source: proto/remote_execution.proto GetTree missing-child contract;
server/remote_cache/content_addressable_storage_server cache flags,
lookupCachedTreeNode and cacheTreeNode; server/buildbuddy_server GetTree adapter;
proto/cache.proto metadata and directory-size fields; app/invocation/
invocation_action_card.tsx and app/util/get_tree.ts tree consumers;
app/invocation/cache_requests_card.tsx metadata consumer; cli/download/download.go.

Final combined invocation (original tests pass; both cached probes fail;
cache-disabled control passes):
https://app.buildbuddy.io/invocation/d9e9b98c-7a7a-49b6-b5f3-2a1e7db3d371

Exact cached-case excerpt, repeated in independent fixtures:

```
after deletion: client_app=true GetTree directories=5 err=<nil>, direct CAS child=NotFound contains_deleted=true
after deletion: client_app=false GetTree directories=5 err=<nil>, direct CAS child=NotFound contains_deleted=true
frontend GetTree after deletion: messages=1 terminal=EOF
```

The frontend adapter probe records stream success only; its directory bytes are
not decoded. The deleted-directory assertion is on both REAPI response bodies.

Verbose cache-disabled control:
https://app.buildbuddy.io/invocation/267be873-ff3c-4d30-8e40-765992355b16

Both app and proxy returned zero directories and FailedPrecondition with
"Digest ... not found"; the frontend adapter returned the same error with zero
messages. No deleted directory was exposed. This verifies that the discrepancy
comes from derived tree caching, rather than a failed delete or missing uploaded
file data. The retained probe asserts the deleted-directory invariant; the
control's exact error is recorded as evidence, not asserted as a normative REAPI
contract.
