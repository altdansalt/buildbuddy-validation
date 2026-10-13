# Local conformance audit

Audited origin/cand/sm1/convergence-streams at ee1988f3eb3ebae9ac89a2287263ce394915d6ec.
Work is confined to audit/convergence-streams-local; do not push this throwaway branch.

Verdict: shallow against the requested EVERY-surface standard, with substantive assertions on the selected surfaces.

## Actual coverage

Both app and cacheproxy ByteStream.Write use identical post-operation checks. Four large repetitive blobs are created; each gets an identical repeat and an alternate identity/zstd representation write (12 writes total). Every existing model entry is checked after every write.

- App/proxy ByteStream.Read: exact logical bytes for full, interior, and EOF ranges, identity and zstd.
- App/proxy CAS.BatchReadBlobs: one response, success status, matching hash/size, exact decompressed bytes with identity/zstd acceptance. Compressor negotiation itself is not strictly asserted.
- App/proxy CAS.FindMissingBlobs: empty post-write list. Initial absence checks only list length one, not returned digest identity.
- App api.v1.GetFile: exact full logical bytes over gRPC and REST, identity/zstd.
- App api.v1.GetFileRange: exact prefix/interior/EOF slice bytes over gRPC and REST, identity only.
- UI /rpc/BuildBuddyService/GetCacheMetadata: HTTP success and digestSizeBytes equality only. storedSizeBytes, lastAccessUsec, lastModifyUsec are returned but not asserted. The UI actually renders the timestamps.
- bb download: successful exit and exact file bytes through both app and proxy, identity resources only. Missing binary fails; runner CLI version is not pinned.
- harness.Write checks completion committed_size (logical size for identity; compressed byte size or allowed -1 short circuit for zstd).

## Gaps and failure handling

Missing surfaces: actual frontend GET /file/download and /file/view; compressed GetFileRange despite proto/api/v1/file.proto documenting compressed responses; direct BuildBuddyService.GetCacheMetadata and cache.service.Cache.GetMetadata gRPC. Structured Directory payloads would permit CAS.GetTree and UI GetTree/GetTreeDirectorySizes assertions, but none are written. QueryWriteStatus exists but the released app intentionally returns Unimplemented and proxy forwards it; this is not an independently functioning state read.

No t.Skip, optional-CLI bypass, swallowed write error, or success-on-error branch found. check returns at the first failing surface; outer polling retries all errors for up to 30 seconds, requiring one complete successful sweep per blob. Transient mismatches disappear, consistent with eventual consistency, but errors are not distinguished from permanent contract failures. Deadline retries overwrite the original mismatch with DeadlineExceeded; local audit instrumentation logs the first mismatch.

The model is a hash-to-original-payload map; check recomputes its requested digest from expected bytes rather than using the stored key. This is a round-trip oracle for immutable CAS content, not a general transition model. There are no deletions in these mutating RPCs; DeleteFile and BatchUpdateBlobs are separate mutation surfaces.

## Expectation perturbations

AUDIT_PERTURB selects exactly one local expectation mutation; no production component or shared harness is changed.

1. count: require two BatchRead responses instead of one. Failed: https://app.buildbuddy.io/invocation/a44ed53d-bf7c-4478-bf0c-8364cfd5a55c Repeat with first-error logging confirmed "batch response count": https://app.buildbuddy.io/invocation/21076fe2-4c0a-4b9e-9211-1590747ad25c.
2. metadata: require digestSizeBytes + 1. Failed with first error UI metadata logical size, actual 1720000: https://app.buildbuddy.io/invocation/d451efc2-af39-4cf2-aad0-62bd91c36e6e
3. model: flip first payload byte in reference model. Failed with first error ByteStream NotFound for recomputed expected digest: https://app.buildbuddy.io/invocation/8d5706bb-a05e-4ff1-96f7-dec976811c25

Result: 3/3 distinct perturbed expectations failed. Unperturbed TestStreamConvergence passed in https://app.buildbuddy.io/invocation/7ef4c2e4-0e29-4b6d-a401-3ec0ba05e203 (aggregate invocation fails because the additional probe detects the omitted API discrepancy).

## New local probe

TestAuditMissingSurfaces uploads via each writer, identity/zstd, then checks UI file/download and file/view in both representations and full logical GetFileRange via gRPC/REST in both representations. All RPCs and HTTP requests target the disposable fixture.

Corrected probe: https://app.buildbuddy.io/invocation/4539f97c-2edc-458d-a005-fbdb04c79cb6

UI download/view identity and zstd returned identical logical bytes in all four write combinations. Identity full-range GetFileRange succeeded. Compressed full-range GetFileRange failed for all four combinations:

    gRPC: FailedPrecondition: passthrough compression does not support offset/limit
    REST: HTTP 500, same FailedPrecondition message

Same compressed-range discrepancy reproduced in https://app.buildbuddy.io/invocation/7ef4c2e4-0e29-4b6d-a401-3ec0ba05e203. That first probe incorrectly used a published loopback URL with WebSession's issuer-only transport, so its UI requests failed within the harness; those errors are rejected as evidence of any product defect. Corrected probe uses f.Web.URL and all UI requests pass. Independent GetFileRange requests were correctly fixture-local in both runs.

Released source corroborates this API discrepancy: enterprise/server/api/api_server.go GetFileRange sends the compressed ResourceName and a positive logical limit directly to cache.Reader; enterprise/server/backends/pebble_cache/pebble_cache.go readerForMetadata rejects compressed passthrough offset/limit. ByteStream performs the transcoding needed for logical ranges; GetFileRange does not. The suite README knowingly excludes this path. This demonstrates an omitted, documented API behavior inconsistency, not app/proxy stale-content divergence or an infrastructure failure.

Both invocations used v2.314.0 pinned images:

- app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
- proxy: gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906

Re-run perturbations using bb remote test //tests/statemachine/convergence-streams/... --test_filter='^TestStreamConvergence$' --test_env=AUDIT_PERTURB=count --nocache_test_results --test_output=errors (substitute metadata or model). Run probe with --test_filter='^TestAuditMissingSurfaces$' --nocache_test_results --test_arg=-test.v --test_output=all. The audit probe deliberately fails on the observed discrepancy; it is retained only on this local throwaway branch.
