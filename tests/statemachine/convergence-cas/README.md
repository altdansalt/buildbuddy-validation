# Batch-written flat CAS convergence

Target: `//tests/statemachine/convergence-cas:go_default_test`.
Run: `bb remote test //tests/statemachine/convergence-cas/... --test_output=errors`.

Released app and cache proxy use the harness defaults (enterprise-v2.314.0):

- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

## Source enumeration

Inspected the released checkout at `~/buildbuddy-src`, especially
`proto/remote_execution.proto`, `proto/api/v1/{service,file}.proto`,
`proto/{buildbuddy_service,cache,resource}.proto`,
`server/remote_cache/content_addressable_storage_server`,
`enterprise/server/content_addressable_storage_server_proxy`,
`enterprise/server/api/api_server.go`,
`server/buildbuddy_server/buildbuddy_server.go`,
`server/cache_server/cache_server.go`, and `cli/download/download.go`.

| Mutation | Read surfaces | Coverage |
| --- | --- | --- |
| CAS.BatchUpdateBlobs on app | app and proxy FindMissingBlobs, BatchReadBlobs, ByteStream.Read | new blobs, mixed existing/new batches, repeated identical batches |
| CAS.BatchUpdateBlobs on proxy | same | new blobs, mixed existing/new batches, repeated identical batches |
| Both writers | app ApiService.GetFile (gRPC and REST), GetFileRange (gRPC and REST), UI `/file/download` and `/file/view`, BuildBuddyService.GetCacheMetadata (gRPC and JSON), `bb download` | exact bytes on file reads; metadata presence for nonempty blobs; file URIs name both local peers; CLI explicitly targets each peer |

Batch CAS is immutable. No in-place update exists: the sequence adds new digests
and repeats writes of identical bytes. ByteStream.Write, compressed streaming,
DeleteFile, action cache, and metadata field semantics belong to other items.
GetTree requires serialized Directory blobs and is not a flat blob read.
Invocation/execution download lists expose build records, not arbitrary CAS
presence, so they do not apply to these flat batch writes.

The reference model initially includes four absent blobs and the canonical
empty CAS blob (logically present without a physical cache record). After each
successful batch response, it marks precisely those blobs present. Both peers'
batch and streaming surfaces must agree with the model within 20 seconds,
polling every 100 ms; RPCs share a five-minute overall bound. Both present and
not-yet-written digests are checked. Public/UI/CLI reads run before writes and after every
operation, including repeats; missing files must return no content and a missing/unavailable error or HTTP
404; direct CAS/range/metadata and CLI missing reads must report NotFound. Empty content is checked as bytes, not Go slice
allocation identity. Physical metadata and positive-length ranges are checked
only for nonempty blobs.

The runner downloads the released bb 5.0.492 Linux binary, verifies SHA256
`e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`, and
removes the outer dispatch credential from its environment. Its component
requests use only the disposable fixture key and loopback endpoint. Public
GetFile wire-compatible protobuf wrappers avoid adding generated upstream
bindings; range and metadata requests add documented protobuf wire fields.

All containers are fixture-local; no component calls go to a shared deployment.
No harness or shared dependency changes are required.

The app pooled file reader prefers its own cache before the URI host; therefore
public/UI reads verify the app backing state, while direct CAS/ByteStream and
CLI reads independently verify each endpoint. The released pooled reader
explicitly sanitizes failed reads to Unavailable (see
`server/remote_cache/byte_stream_client/byte_stream_client.go`), including
missing files. The test accepts this documented error mapping without treating
it as a state divergence.

UI download/view absence is not an independent flat-CAS oracle: on a cache
miss those handlers fall back to invocation blob storage and require an
invocation ID. This item checks their exact bytes for accepted writes; absence
is checked through the CAS, public file/range APIs, metadata, and CLI instead.
No invocation records are fabricated to exercise that unrelated fallback.

## Verification

Final targeted remote test passed in 45.5 seconds:
https://app.buildbuddy.io/invocation/77786fa2-06b1-451f-bd31-592014480ed6
(outer runner: https://app.buildbuddy.io/invocation/9353d677-3f93-4bce-882a-6971e83d2a3e).
No accepted-write divergence was observed. Earlier development runs corrected
empty-slice assertions, REST request encoding, and source-documented error and
UI fallback expectations; they are not product findings.
