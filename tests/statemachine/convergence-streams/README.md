# Streamed content convergence

Target: `//tests/statemachine/convergence-streams:go_default_test`.
Released fixture defaults: app v2.314.0, digest
`sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`;
proxy v2.314.0, digest
`sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`.
Actual image inspection metadata is preserved by the harness.

## Mutation and read inventory

| Mutation | Reads after each operation | Transition |
| --- | --- | --- |
| `google.bytestream.ByteStream.Write`, app identity/zstd | App and proxy ByteStream identity/zstd full and logical ranges; CAS BatchReadBlobs identity/zstd and FindMissingBlobs; app api.v1.GetFile gRPC/REST identity/zstd; GetFileRange gRPC/REST identity; UI GetCacheMetadata logical size | Absent → create → repeat → repeat with alternate representation |
| `google.bytestream.ByteStream.Write`, proxy identity/zstd | Same surfaces | Absent → create → repeat → repeat with alternate representation |

Every completed write updates a hash-to-logical-content reference model. All
model entries are read after each operation, with a 30-second convergence
deadline and timer-driven bounded polling. Payloads exceed a ByteStream message
and the auto-compression threshold; only decompressed bytes are compared. CAS
responses must also preserve the digest and logical size. Nested instance names,
interior ranges, prefix ranges and ranges ending exactly at EOF are exercised.
`bb download` additionally checks every model blob through both endpoints after
**each** operation. The hosted runner supplies `bb`; absence fails the convergence
check explicitly. Its version is runner-provided rather than pinned by this area.


CAS content is immutable: updates to logical content create new digest entries.
ByteStream offers no delete RPC. The separately gated `api.v1.DeleteFile` and CAS
BatchUpdateBlobs belong to different mutation surfaces and are outside this
streamed-write item. QueryWriteStatus is intentionally excluded. Proxy does not
register the app API/UI services. GetFileRange requests use identity URIs because
Pebble explicitly does not support offset/limit passthrough compression; compressed
logical ranges are exercised using ByteStream, which transcodes them.

## Released source inspected

- `server/remote_cache/byte_stream_server/byte_stream_server.go`: Write completion,
  short-circuit semantics, logical offset/limit and zstd transcoding.
- `enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go`:
  local/remote reads and write forwarding.
- `proto/api/v1/file.proto`, `enterprise/server/api/api_server.go`:
  GetFile and GetFileRange wire contracts, authentication and REST handlers.
- `server/libmain/libmain.go`: external file API registration.
- `proto/cache.proto`, `proto/resource.proto`,
  `server/buildbuddy_server/buildbuddy_server.go`: UI cache metadata.
- `enterprise/server/backends/pebble_cache/pebble_cache.go`: logical versus stored
  size and compression range restrictions.
- `enterprise/server/cmd/cache_proxy/cache_proxy.go`: proxy service registration.
- `cli/download/download.go`: direct digest download delegates to ByteStream.

The small wrapper-message adapter matches the released file API wire fields;
it avoids building BuildBuddy or changing shared generated-proto dependencies.
