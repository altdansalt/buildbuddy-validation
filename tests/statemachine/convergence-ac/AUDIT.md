# Local rigor audit

Candidate: origin/cand/sm1/convergence-ac. Audit changes are local only.

Verdict: shallow relative to the requested EVERY-surface coverage, with real
assertion teeth rather than a hollow always-green test.

## What is asserted

- UpdateActionResult: mutation response and both app/proxy GetActionResult are
  proto-equal to the model after removing ExecutionMetadata. All eight inline
  option combinations are sent; only output-file inlining changes the expected
  result. Released source ignores stdout/stderr inline flags, and the test
  encodes that behavior. App /file/view and /file/download AC bytes are decoded
  and compared. Initial app/proxy AC absence is asserted.
- BatchUpdateBlobs and ByteStream.Write: both app/proxy FindMissingBlobs must
  report no missing revision blobs; BatchReadBlobs must return three responses
  with success status and model bytes; ByteStream.Read and bb download must
  return exact bytes. App /file/view and /file/download return exact CAS bytes.
  These checks happen after the AC mutation, not immediately after each upload.
  The Action, Command and initial empty Directory uploads are not included in
  the explicit CAS/HTTP/CLI byte oracle.
- Metadata: app BuildBuddyService.GetCacheMetadata checks stored size > 0 and
  mtime > 0; AC mtime must strictly increase. HTTP GetCacheMetadata checks ONLY
  stored size equality to that gRPC response. CAS additionally checks digest
  size on the app and Cache.GetMetadata on both app/proxy, and positive stored
  size. It does not assert exact stored size against serialized model bytes,
  HTTP mtime/digest size/access time, cross-endpoint mtime agreement, or proxy
  AC metadata (the proxy uses salted variant keys and CAS pointers).

## Gaps and pass hazards

Released proto/cache.proto, proto/buildbuddy_service.proto,
server/buildbuddy_server/buildbuddy_server.go and
app/invocation/invocation_action_card.tsx expose GetTree and
GetTreeDirectorySizes; neither is covered. REAPI CAS.GetTree also is not called.
cli/download/download.go has Directory interpretation and recursive extraction,
not exercised by the raw-blob CLI checks. enterprise/server/api/api_server.go
and proto/api/v1/file.proto expose GetFile/GetFileRange, including HTTP
/api/v1/GetFile and /api/v1/GetFileRange, which are omitted. ByteStream read
ranges/compression and BatchRead compression are omitted. QueryWriteStatus is
not checked, though the released app explicitly returns Unimplemented, so
successful completion is not a valid oracle for that RPC on this release.

No skips, swallowed read errors, or early successful returns were found. Polling
is bounded to 15 seconds and followed by three immediate checks. These three
checks do not prove sustained stability or simultaneous consistency: surfaces
are read serially. The polling permits temporary mismatches by design and
short-circuits later surfaces until earlier ones pass.

The model exercises one output file, executable toggles and raw/digest
stdout/stderr transitions; exit code remains zero, no output directories,
symlinks or user-supplied ExecutionMetadata are exercised. Stripping all
ExecutionMetadata hides discrepancies in that entire submessage. CAS digests
are always new, and old blobs/results are not audited after replacements.
BatchRead checks count and each returned digest's bytes, but not digest set
membership, uniqueness, size or compressor: three copies of one valid response
could satisfy its oracle. Metadata surfaces can agree on an incorrect stored
size. Thus green does not establish the stated universal conformance intent.

## Expectation perturbations

Each was applied alone to the original test, run with bb remote test
//tests/statemachine/convergence-ac/... --nocache_test_results, and restored.
All compiled and failed at the intended assertion/mismatch: 3/3.

1. Initial AC expected OK instead of NotFound: actual NotFound.
   https://app.buildbuddy.io/invocation/934586a8-76ca-406e-9d2b-17918e2cc3c0
2. BatchRead expected one extra response: first mismatch logged, then bounded
   convergence failed.
   https://app.buildbuddy.io/invocation/f05eb0f6-e5a9-47d9-93a0-6536831a30db
3. Read-only reference clone expected ExitCode=99 (write remained zero): first
   mismatch explicitly showed expected 99, then bounded convergence failed.
   https://app.buildbuddy.io/invocation/7a7ac6de-1e68-40f7-a04e-16aa3664f0cf

## Additional check

TestAuditMissedTreeSurfaces uploads nested directories via both BatchUpdateBlobs
and ByteStream.Write, alternating app/proxy writers. It checks exact root/child
Directory protos through both CAS.GetTree endpoints and the HTTP UI
GetTreeDirectorySizes response for both cumulative byte sizes and child counts.
The original suite is restored and also runs in the same invocation.

Result: PASS, no divergence found in the added checks; original suite also PASS.
https://app.buildbuddy.io/invocation/70eaf0aa-fb88-4e86-a852-e21164005a4e

Images: released enterprise-v2.314.0 defaults, app
sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843,
proxy sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906.
No shared files changed; no push or PR.
