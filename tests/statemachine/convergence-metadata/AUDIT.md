# Local rigor audit

Verdict: shallow, with real assertion teeth. No new divergence observed.

Three perturbations were applied together in independent subtests: batch response
count 1 -> 2; splice chunk count 2 -> 3; AC expected stdout -> incorrect bytes.
All three failed at their intended assertions, including the AC polling timeout:
https://app.buildbuddy.io/invocation/fbac82d6-8f70-4348-875e-fc7da63ec1dc
Perturbations were then restored (snapshot retained at /tmp/metadata-audit-perturbed.go).

Original coverage:
- BatchUpdateBlobs and ByteStream.Write identity/zstd: app/proxy identity
  ByteStream bytes, BatchReadBlobs count/status/bytes, FindMissingBlobs empty;
  app/proxy Cache.GetMetadata key size and positive stored size/timestamps;
  app BuildBuddyService.GetCacheMetadata gRPC key size, HTTP key size and
  positive stored size. bb download checks proxy bytes, conditionally installed.
- UpdateActionResult: app/proxy GetActionResult with InlineStdout=true compares
  stdout only; app metadata surfaces as above. No proxy AC metadata check.
- SpliceBlob: app/proxy SplitBlob ordered two chunk identities and ByteStream
  bytes only. No original whole-blob existence, HTTP, CLI, or metadata check.
- RegisterChunkMapping not called; source returns Unimplemented. Source also
  returns Unimplemented for QueryWriteStatus; no public cache-delete RPC.

Gaps: UI /file/download CAS and actioncache content; directory-shaped CAS data
and REAPI/UI GetTree plus UI GetTreeDirectorySizes; compressed read variants;
AC default/non-inline reads and stderr/exit/output fields. AC replacement uses
an unchanged action digest size; stored size merely >0 cannot detect stale
metadata. Metadata times only >0 cannot detect stale times. Proxy AC metadata
uses request-hashed pointer keys and splice metadata is manifest-based, so naive
whole-key comparisons would be invalid. Read-through before proxy metadata can
heal missing local state; no independent convergence observation. Polling is
bounded and preserves assertion errors; early returns after asserted RPC errors
do not swallow failures. CLI Skip is explicit; every CLI subtest actually passed
in the verbose original-suite rerun below.

Added TestAuditMissingSurfaces verifies app HTTP CAS bytes after each mutation
through either writer; HTTP AC stdout/stderr/exit after create/replace; splice
FindMissingBlobs on both readers before read-through, then HTTP reconstruction.
All added checks passed on pinned v2.314.0 fixture images:
https://app.buildbuddy.io/invocation/7c3e50f5-2ca4-42b2-a0eb-98c935a70f2a
Exact image digests are in README.md and invocation logs.

Initial added-check run had setup errors (missing API-key HTTP header and a
5 MiB BatchReadBlobs request above the 4,000,000 byte default limit). These are
not product divergences. That run also confirms the original suite passed:
https://app.buildbuddy.io/invocation/77489fd9-1cda-4dff-9fc8-9811536d52cc
The corrected checks authenticate HTTP using the disposable fixture API key
and omit the unsupported oversized batch request.

Local throwaway branch only; no push, PR, or shared harness/dependency changes.
