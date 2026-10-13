# App/proxy deletion and recreation conformance

Target: `//tests/statemachine/convergence-deletes:go_default_test`.
Run with `bb remote test //tests/statemachine/convergence-deletes:go_default_test --test_output=all`.

Uses the harness's digest-pinned enterprise-v2.314.0 images:

- App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

## Mutation/surface inventory

Inspected the released source in `~/buildbuddy-src`, not built from source.

| Mutation | Read surfaces checked after transitions | Contract |
| --- | --- | --- |
| CAS.BatchUpdateBlobs (app and proxy) | app/proxy FindMissingBlobs, BatchReadBlobs, ByteStream.Read; app GetFile/GetFileRange; metadata gRPC and web HTTP; UI file/download and file/view; bb download when available | Newly created or recreated bytes match their SHA256 digest everywhere. |
| ByteStream.Write (app identity, proxy zstd) | Same CAS surfaces | Repeating an immutable write preserves content; recreation restores backing visibility. |
| AC.UpdateActionResult (app and proxy) | app/proxy GetActionResult, app metadata gRPC/web HTTP, public HTTP GetFile and UI downloads with actioncache URI | Mutable stdout revision converges, including after delete/recreate. Server-generated execution metadata is outside the reference revision. |
| api.v1.ApiService.DeleteFile, CAS and AC, gRPC and REST; repeated absent deletes | All applicable surfaces above; CAS GetFile streaming gRPC and REST range reads included | Idempotent success; app becomes absent, AC proxy eventually absent; warmed immutable CAS may remain locally. |

Both empty and named instance namespaces are tested. A minimal successful BES
invocation is created for UI download requests so CAS misses can validly fall
back to the invocation artifact store (where this test stores no backup blob). Valid Command, Directory,
and Action protos are uploaded before AC mutations. SHA256 digests and valid
resource names are used throughout. REST CAS deletion also uses the supported
compressed resource-name alias. Every modeled mutation is followed by reads.
The reference model records backing CAS existence and mutable AC stdout/exit
code; a separate warmed-cache bit models allowed local immutable retention.
AC polling is bounded to ten seconds, uses ticker-driven retries rather than
sleeps, and checks default revalidation and a supported 200ms client TTL.

## Deliberately different cache views

**There is no app-to-proxy CAS delete invalidation guarantee in this configuration.**
Deletion is not a purge of independent immutable replicas, and retention is not
classified as a product defect. With default FindMissing TTL zero, a proxy
FindMissingBlobs forwards to the backing app, so it can report missing while
BatchReadBlobs and ByteStream.Read still return digest-valid warmed local bytes.
Recreation uses identical content for the same CAS digest, never different bytes.

Proxy `cache.service.Cache/GetMetadata` describes local storage. For CAS it can
still return metadata after app deletion. Proxy AC entries are stored under a
salted hash of GetActionResult requests, with pointers into CAS; metadata queried
using the original action digest is absent. This does not contradict the logical
GetActionResult view, which revalidates with the app by default.

The public pooled GetFile streaming gRPC method sanitizes misses as `Unavailable`;
its HTTP handler returns 404. Direct ByteStream and range gRPC reads return
`NotFound`. The assertions distinguish response contracts from presence state.

## Source anchors and reachability limits

- `proto/api/v1/{file,service}.proto`,
  `enterprise/server/api/api_server.go`: DeleteFile gate
  `--enable_cache_delete_api=true`, CAS/AC resource parsing and idempotency;
  public GetFile, GetFileRange and HTTP handling.
- `server/libmain/libmain.go`: `/api/v1/` REST mapping, public cache metadata RPC.
- `server/buildbuddy_server/buildbuddy_server.go`: GetCacheMetadata and UI
  `/file/download`, `/file/view`, including actioncache URLs.
- `server/cache_server/cache_server.go`, `proto/{cache,resource}.proto`:
  metadata resource representation and local storage semantics.
- `enterprise/server/content_addressable_storage_server_proxy/`:
  default FindMissing pass-through; local-first BatchRead and write-through upload.
- `enterprise/server/byte_stream_server_proxy/`: local-first immutable reads.
- `enterprise/server/action_cache_server_proxy/`: salted request cache keys,
  authoritative revalidation, client-opted TTL and remote UpdateActionResult.
- `server/util/platform/platform.go`: supported
  `x-buildbuddy-platform.cache-proxy-action-cache-ttl` request header.
- `server/remote_cache/byte_stream_client/byte_stream_client.go`: public file
  reads and miss sanitization; URLs point to the app's fixture-internal address.
- `cli/download/download.go`: blob download uses ByteStream; the CLI accepts
  `/blobs/HASH/SIZE` but not named-instance paths. CLI assertions cover the empty
  namespace on both components and explicitly log if the runner lacks `bb`.

The proxy does not register a public DeleteFile service. No proxy-originated
DeleteFile request is invented. `proto/distributed_cache.proto` exposes Delete
for distributed peers, and `enterprise/server/backends/distributed/distributed.go`
registers it only with an explicitly configured distributed listener. The standard
harness uses standalone Pebble and does not expose that listener. Testing that
path needs a coordinator-owned distributed fixture/port extension; this area
makes no shared harness edits and does not treat it as covered.

UI metadata availability is checked through both public gRPC and the real
cookie-authenticated JSON protolet API. Metadata timestamps are not compared:
reads legitimately update access times. Public file bodies and AC stdout
revisions are compared, not just HTTP success codes.

## Released-image verification

- [Initial complete green run](https://app.buildbuddy.io/invocation/ed8bc913-5b88-4c2e-929e-101beab4b5a3).
- [Final uncached run, including AC streaming gRPC](https://app.buildbuddy.io/invocation/a2240738-8f50-4e05-93e6-5016d2c12d13): both namespaces passed in 50.26 seconds; `bb` was available, so empty-namespace CLI checks ran against both fixture components. No confirmed product defect.

The distributed-peer Delete path remains uncovered for the fixture reachability
reason above. No cache invalidation guarantee is inferred from the CAS retention
observation. Earlier setup failures were resolved against released source
contracts (generated execution metadata, public-file miss sanitization, and UI
invocation-artifact fallback); they are not product findings.
