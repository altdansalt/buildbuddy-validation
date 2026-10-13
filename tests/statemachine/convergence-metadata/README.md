# Released resource metadata state machine

Target: `//tests/statemachine/convergence-metadata:go_default_test`.

The reference models are immutable CAS bytes indexed by SHA256 digest, ordered
chunk digests for spliced blobs, and the latest AC stdout bytes indexed by action
digest. Every mutation is driven through app and proxy, with fresh creates and
successful repetitions; AC replacements use distinct serialized sizes. Content
read-through precedes local proxy metadata assertions. Polling is bounded to 10
seconds at 100ms intervals; RPC work has a four-minute context.

## RPC inventory and read surfaces

| Mutation | Read surfaces | Coverage |
| --- | --- | --- |
| CAS.BatchUpdateBlobs | app/proxy BatchReadBlobs, FindMissingBlobs, ByteStream.Read, Cache.GetMetadata; app UI GetCacheMetadata over gRPC and JSON HTTP | creation and repeat |
| ByteStream.Write (identity and zstd) | same surfaces | creation and repeat |
| AC.UpdateActionResult | app/proxy GetActionResult; app Cache.GetMetadata and UI GetCacheMetadata over gRPC and HTTP | creation, replacement, repeat |
| CAS.SpliceBlob | app/proxy SplitBlob and ByteStream.Read | two preuploaded chunks totaling 5 MiB, creation and repeat |
| CAS.RegisterChunkMapping | none | released server returns Unimplemented; not sent |

`bb download` verifies CAS bytes through the local proxy when `bb` is installed on
the runner; it explicitly skips if absent. A pinned CLI runfile is needed for
unconditional CLI coverage. There is no dedicated CLI metadata reader.

There is no public delete RPC for cache resources, so deletion is not simulated
with eviction or fault injection. CAS update means an idempotent rewrite of
identical content, or a fresh identity for new content. Metadata request identity
is tested with separate namespace, hash, digest size and cache type; responses do
not echo resource identity. Stored size and positive access/modify timestamps are
checked independently per storage surface; they are deliberately not equated
between app and proxy. No exact access-time advancement is required because
Pebble batches access updates.

## Released representation boundaries

The proxy hashes the complete GetActionResult request into its local AC key and
stores a CAS-resource pointer under that key. App action-digest metadata and
proxy local AC metadata therefore do not expose the same resource. AC content
convergence is asserted; proxy action-digest metadata is not required to exist.
SpliceBlob stores a manifest under a reserved AC namespace, rather than creating
a materialized CAS metadata record at the whole-blob digest. SplitBlob and reads
expose the manifest; GetMetadata does not resolve manifests. This coverage does
not claim whole-blob metadata agreement for manifest-only blobs.

Cache.GetMetadata/GetCacheMetadata responses in this release have only stored
size, logical digest size, access and modify timestamps: encryption and chunk
information are not response fields. Encryption requires additional fixture key
management setup and is not exercised. Chunk mapping agreement is checked with
SplitBlob. GetChunkMapping and RegisterChunkMapping are unimplemented.

## Source enumeration (v2.314.0)

- `proto/cache.proto` GetCacheMetadataRequest/Response; `proto/cache_service.proto`
  and `proto/buildbuddy_service.proto` read RPC declarations.
- `proto/remote_execution.proto`: CAS mutations, AC.UpdateActionResult and chunk
  mapping RPC declarations; ByteStream.Write uses the standard ByteStream proto.
- `server/cache_server/cache_server.go`: metadata reads local cache only.
- `server/buildbuddy_server/buildbuddy_server.go`: UI delegates to Cache client.
- `enterprise/server/backends/pebble_cache/pebble_cache.go`: metadata field meanings.
- `enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go`:
  request-hashed local AC identity, CAS pointer, remote source of truth.
- `server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go`
  and `server/remote_cache/chunking/chunking.go`: splice manifest storage,
  SplitBlob lookup and unimplemented streaming mapping APIs.
- `enterprise/server/content_addressable_storage_server_proxy/` and
  `cli/download/download.go`: proxy forwarding and CLI byte reader.

Images are harness defaults, enterprise-v2.314.0:

- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

Only fixture-local services are called. No cloud dispatch credential is read by
the test. Private metadata messages use a small wire codec with release proto
field numbers to avoid introducing a shared generated-proto dependency.

The chunk fixture exceeds the default 4 MiB reconstruction threshold; tiny
manifest-only blobs are outside that advertised storage regime. An initial tiny
chunk probe was rejected as a test configuration error, not a product finding.

Validation: CAS/AC passed at
https://app.buildbuddy.io/invocation/a6b2ac62-b603-4ae6-9d58-64e45332510a;
full target including realistic splice reads passed uncached at
https://app.buildbuddy.io/invocation/7367b15b-4f20-452e-96db-19f1214127ed.
