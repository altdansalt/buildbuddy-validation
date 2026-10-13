# Public cache deletion state machines, round 2

Images: pinned enterprise-v2.314.0 app and cache proxy, with exact digests in
FINDING-cacheproxy-warmed-delete.md and fixture artifacts. Only local fixture
endpoints and disposable org API keys are used. No shared dependency changes.

## Source-backed surface inventory

Inspected the released checkout at ~/buildbuddy-src:

- proto/api/v1/{service,file}.proto and enterprise/server/api/api_server.go:
  DeleteFile gRPC/JSON HTTP, streamed GetFile gRPC/HTTP, GetFileRange gRPC/HTTP.
- proto/remote_execution.proto, server/remote_cache/{action_cache_server,
  content_addressable_storage_server,byte_stream_server}: exact-digest
  FindMissingBlobs and BatchReadBlobs, identity/zstd ByteStream Read and ranges,
  REAPI GetTree, full/inline GetActionResult and conditional result digest.
- proto/{cache,resource,buildbuddy_service}.proto and
  server/buildbuddy_server/buildbuddy_server.go: metadata sizes/access/modify
  timestamps, tree directory sizes, UI GetTree streaming, /file/view and
  /file/download. Both routes stream CAS or validated serialized AC. Dangling
  AC bytes still exist in metadata but the serialized readers validate outputs
  and miss, like REAPI GetActionResult.
- app/service/rpc_service.ts and app/util/get_tree.ts: the frontend sends real
  application/proto+prefixed requests to /rpc/BuildBuddyService/GetTree. The
  tests parse length-prefixed responses and gRPC status headers/trailers; JSON
  HTTP and gRPC metadata are also checked. app/invocation/invocation_action_card.tsx
  consumes trees, directory sizes, CAS/AC bytes and stdout/stderr bytes.
- app/target/target_artifacts_card.tsx, proto/zip.proto and
  server/remote_cache/byte_stream_client: GetZipManifest and /file/{view,download}
  with a manifest-entry z parameter add ranged ZIP member reads. Valid ZIP
  fixtures exercise these through rewrite/delete/recreation.
- enterprise/server/{byte_stream_server_proxy,content_addressable_storage_server_proxy,
  action_cache_server_proxy}: proxy local hits, remote missing lookup, and AC
  remote validation. Proxy offers REAPI mutations; public DeleteFile belongs
  to the app. A proxy DeleteFile call would be unsupported, not a valid mutation.
- cli/download/download.go: blob download and Directory extraction are checked
  using a precreated output directory. Nested instance CLI syntax is unsupported
  by this released parser: it prepends /blobs/ unless input starts with /blobs,
  so a nested resource cannot round-trip. Nested resources are covered with RPCs
  and UI endpoints; no arbitrary CLI failure is accepted as an absence oracle.

Invocation raw_json/raw_proto, buildlog/runlog, GetAction, GetExecutionDownloads,
and execution streaming expose BES/blobstore or execution records, not cache
entries. Cache mutation does not mutate those stores; they are not cache-state
oracles. QueryWriteStatus is Unimplemented on the released app, not a usable
presence reader. No unconditional skips or error-to-success fallbacks are used.

## Mutation matrix

| Mutation | Checked surfaces | Result |
| --- | --- | --- |
| BatchUpdateBlobs | App exact missing/batch, ByteStream identity/zstd, GetFile gRPC/HTTP, range gRPC/HTTP, UI view/download, metadata sizes/timestamps gRPC/JSON; all seeded CAS checked after each upload. Directory/ZIP projections in their sequences; app/proxy four-client reads in cross-client sequence. | Pass |
| ByteStream.Write identity/zstd | App full byte/range/compressed/UI/metadata matrix; retained CAS/AC; directory tree/sizes and CLI extraction; ZIP manifest/member readers; four-client app/proxy convergence | Pass |
| UpdateActionResult | Full proto equality, inline output plus stdout/stderr flags, matching/mismatching conditional result digest, validated serialized AC GetFile/UI, metadata, CAS readers/tree/sizes/CLI; four app/proxy clients | Pass |
| DeleteFile AC gRPC/HTTP, repeated and recreate | Validated AC and serialized readers miss; metadata missing; unrelated AC and all CAS retained; app/proxy AC delete and recreation converge for both org keys | Pass |
| DeleteFile CAS gRPC/HTTP, repeated and recreate | App full matrix; dangling AC validated readers miss while metadata remains; recreation restores AC reads; Directory deletion and ZIP deletion projections | Pass on app; warmed proxy divergence |

Conditional AC test enables cache.check_client_action_result_digests=true;
otherwise that extension is ignored by default. Delete tests enable the required
non-default enable_cache_delete_api=true. Compression expectations retain the
standard Pebble 100-byte auto-compression threshold (applied to resource digest
size, including AC keys), and compare exact stored bytes to the zstd model.
Small uncompressed payloads and larger compressed Action/AC/ZIP resources are
included. Metadata timestamps are positive, modify times bounded by current
wall time, and CAS modify timestamps agree between gRPC and JSON HTTP.

## Sequences and timing

The app model runs empty and nested instances, multiple unrelated CAS/AC keys,
create→update→update→delete→re-create, repeated valid updates/deletes, deletion
with dangling AC outputs, and identity/zstd writes. Seeded Action, Command,
Directory and ordinary blobs are checked after every individual upload and
again after every modeled AC/CAS mutation. Directory and ZIP-specific tests
check every step of their own state sequences.

CrossClientWrites provisions a second CACHE_WRITE key in the same org. Writers
alternate among app/proxy endpoints and both keys; all four clients compare
all modeled CAS and AC state after every upload/write/update/delete/recreation.
Convergence polling is bounded to 15 seconds, followed by three full matrix
checks 100ms apart. Logged convergence time is an observation upper bound
including traversal of the matrix, not the exact internal commit latency.
Self-dialed UI metadata/directory-size startup errors retry only Unavailable
for up to 15 seconds; contradictory presence/data is never retried there.

Warmed proxy CAS deletion is isolated in bug-tagged warmed_proxy_delete, preserving
its failing desired convergence expectation so default CI remains green. Two
fresh remote reproductions show divergence at 0/5/30/60 seconds. It is persistent
within this window; permanence and a documented remote invalidation guarantee
are unknown. See the finding for evidence and prerequisites.

The full matrix is distributed across focused tests: the four-client proxy
sequence uses REAPI reads, while UI/metadata/CLI projections are exercised by
app-focused sequences. This does not claim that every UI/CLI projection is
sampled under each of the four client identities. Nested CLI instance support
would require a CLI product fix; no harness changes are requested.

## Validation

Passing expanded suite before the last per-seed metadata/range strengthening:
https://app.buildbuddy.io/invocation/34ddf979-97f6-4715-8608-c1fd03ceba0b
Cross-client write observations in that run: 9–42ms; stable on three checks.
Final exact test sources: all four normal tests pass, and the isolated bug
reproduces in both fresh runs (the combined invocation deliberately exits with
failure because the bug target is enabled):
https://app.buildbuddy.io/invocation/020a85fa-8e09-4424-a91e-754e7b3bc6c9
Normal target duration 153.2s under concurrent repro load; repro 82.4–82.5s
including fixture startup, with a 60s post-delete observation window. Cross-client
convergence observations: 11–61ms, stable on three subsequent checks.
Default suite excluding bug also passed:
https://app.buildbuddy.io/invocation/c9279340-e8bf-4a12-ba14-8a05367692dc
No PR was opened per task instructions.
