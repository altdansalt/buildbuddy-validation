# ByteStream publication conformance

Released source inspected at `~/buildbuddy-src` (v2.314.0):

| Mutation | Readers | Sequence |
| --- | --- | --- |
| `google.bytestream.ByteStream.Write` identity and zstd | app + proxy ByteStream.Read (identity and zstd), CAS FindMissingBlobs, BatchReadBlobs (zstd accepted), cache.service.Cache.GetMetadata | absent → chunked completed write → repeat same encoding → repeat other encoding; compare immutable reference model after every completion |
| same Write | app BuildBuddyService.GetCacheMetadata (gRPC and cookie-auth JSON protolet), api.v1.GetFile, GetFileRange, HTTP /file/download and /file/view | compare digest size, whole content and interior range after each completion |
| same Write | bb download against each fixture component | compare all model blobs after the sequence, if runner provides bb |

Sources: `proto/resource.proto`, `proto/cache.proto`, `proto/cache_service.proto`,
`proto/api/v1/file.proto`, `proto/api/v1/service.proto`,
`server/remote_cache/byte_stream_server/byte_stream_server.go`,
`server/cache_server/cache_server.go`, `server/buildbuddy_server/buildbuddy_server.go`,
`server/libmain/libmain.go`, `enterprise/server/cmd/cache_proxy/cache_proxy.go`,
`enterprise/server/api/api_server.go`, and `cli/download/download.go`.

ByteStream has no update/delete RPC: digest-addressed bytes are immutable, so
repeat Write is the valid idempotency transition. api.v1.DeleteFile belongs to
another service and is outside this item's upload-publication mutation scope.
QueryWriteStatus is intentionally excluded (released implementation returns
Unimplemented for unfinished writes). The cache proxy does not register the app
UI or api.v1 file services; those are checked on the app after proxy writes.

The model changes only after successful completion. FindMissingBlobs is polled
with a 10-second bound before checking complete bytes on both components.
Identity committed size must match logical size; zstd committed size must match
compressed size or the protocol's -1 already-present sentinel. Upload messages
are 8191 bytes, ensuring multiple messages for both encodings. Server early
completion is accepted for already-present blobs.

Default pinned images are app
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
and proxy
`gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
(enterprise-v2.314.0). Harness artifacts record actual image/configuration.

Coverage limit: CLI subtest explicitly skips if bb is unavailable on the runner;
a pinned runner CLI artifact would be needed to guarantee that surface without
changing shared dependencies. No product divergence is waived in these tests.
