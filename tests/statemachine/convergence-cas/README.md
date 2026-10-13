# CAS cross-surface state machine

Targets: //tests/statemachine/convergence-cas:go_default_test (coverage),
and :delete_convergence_repro (bug-tagged isolated deletion mismatch).
Only fixture-local released v2.314.0 app/proxy containers receive component
requests. Exact image digests are recorded in FINDING-app-delete-stale-proxy.md
and harness test logs. The CLI is released bb 5.0.492, SHA256
e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9.

## Source and surfaces

Inspected proto/{remote_execution,cache,cache_service,buildbuddy_service,resource}.proto,
proto/api/v1/{file,service}.proto; server/{cache_server,buildbuddy_server,http/protolet},
server/remote_cache/{directory_size,content_addressable_storage_server,byte_stream_client};
enterprise/server/{api,content_addressable_storage_server_proxy,byte_stream_server_proxy};
cli/download/download.go; app/service/rpc_service.ts,
app/invocation/{cache_requests_card,invocation_action_card}.tsx and app/util/get_tree.ts.

| Mutation | Surfaces checked after each step | Result |
| --- | --- | --- |
| BatchUpdateBlobs app and proxy | Both peers FindMissingBlobs, BatchReadBlobs identity/zstd, ByteStream full/partial reads; Cache/GetMetadata after hydration; app public GetFile gRPC/REST and GetFileRange full/partial gRPC/REST; UI download/view bytes; GetCacheMetadata gRPC/JSON sizes/timestamps; peer-targeted bb download | Coverage |
| Directory batch writes app and proxy, repeats | Above byte surfaces plus both CAS.GetTree, BuildBuddyService.GetTree gRPC and actual frontend prefixed HTTP streaming, GetTreeDirectorySizes gRPC/JSON totals/counts, both peers bb download --output_directory | Coverage once the modeled root exists |
| ByteStream.Write app/proxy identity/zstd repeated valid writes | Same complete flat and directory projections | Coverage |
| App compressed batch, nonempty instance | Forwarded presence and cold local metadata before any content reads; exact app/proxy bytes and metadata success after hydration | Focused variant coverage |
| DeleteFile app after proxy write/hydration | Both presence RPCs, proxy ByteStream and batch reads, final app/proxy bytes; app recreation readback | Confirmed mismatch >20s, non-default enable_cache_delete_api flag |

The main reference model tracks initially absent payload and Directory blobs,
plus canonical virtual empty CAS. Six alternating writer batches mix new and
existing objects and repeat every operation. Two distinct API keys in the same
org use different peer clients. Every accepted batch and streaming mutation
runs the complete applicable model comparison, not only final snapshots.
CAS is immutable: valid updates repeat identical content; changed content
necessarily uses another digest.

Convergence is bounded to 20s, followed by four consecutive successful scans
100ms apart. Logs report scan duration including the stability window.
The suite preserves the last real mismatch rather than overwriting it with
a deadline error. Presence lists reject unsolicited digests; batch response
count, duplicate hashes, digest sizes, status, compressor and exact decoded
bytes are checked.

## Oracle boundaries

Proxy local metadata is NOT a remote presence oracle. Before reads, it can be
NotFound for accepted app writes. TestColdProxyAndInstance asserts that local
metadata remains absent for five scans over one second while forwarded
presence immediately succeeds. Then content hydration and local metadata are
checked separately. This explicitly measures demand-driven availability;
successful post-hydration checks do not claim passive replication.

Public/UI calls run on app. Peer URIs are tested, but the pooled reader can
prefer app storage; these are app-backed projections, not independent proxy
oracles. Direct peer CAS, ByteStream, metadata and CLI address each peer.
No claim is made that app and proxy physical stored sizes or access timestamps
must be identical. Metadata checks validate digest size and positive stored
size/timestamps; the restored audit probe additionally checks exact small-blob
stored sizes. Empty CAS is virtual and has no physical metadata.

UI missing downloads can fall back to invocation blob storage and require an
invocation ID; absence is instead asserted through direct/public/metadata/CLI
surfaces. Invocation artifacts, logs, cache scorecards and execution-download
lists need build records and do not expose arbitrary CAS mutation state.
QueryWriteStatus is explicitly Unimplemented in the released app.
Zip extraction and raw-JSON rendering are content-type interpretations of
downloaded blobs; arbitrary flat payloads are not valid zip/protobuf inputs.
DeleteFile is not registered on the cache proxy, so deletion is app-targeted.

## Findings and validation

See FINDING-app-delete-stale-proxy.md for two independent remote reproductions.
No shared harness or dependency changes, PR, or upstream report are required.
The local auditor's original expectations and metadata/partial-range probe
were restored from audit/convergence-cas-local; no perturbations were retained.

After direct convergence, the HTTP/UI/CLI and tree projections run twice
without an intervening mutation to detect regression beyond a single matching
scan. Direct metadata NotFound is also checked on both peers for every modeled
absent digest.

Further source enumeration found SplitBlob/SpliceBlob (chunk-state mutations,
not exercised here; owned by cas-chunk-state). GetChunkMapping and
RegisterChunkMapping explicitly return Unimplemented in this release.
This suite therefore does not claim conformance for every CAS extension RPC.

Final targeted default suite passed in 165.1 seconds:
https://app.buildbuddy.io/invocation/f775e039-6681-454f-9218-3969c708cf64
(outer runner https://app.buildbuddy.io/invocation/3bab6b49-3763-4329-824c-bfa4e8aaa213).
Earlier complete transport/variant run also passed:
https://app.buildbuddy.io/invocation/89b88e0f-db71-4b9a-8d68-b4f8dd480996.
Observed direct write convergence scans in development were 0.313–0.327s,
including the required 300ms stability interval; each final step logs its own
duration. This measures client-visible availability after content hydration,
not passive replication. Cold metadata remained absent over the separate 1s
presence-only probe.
