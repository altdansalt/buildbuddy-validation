# CAS batch-write state conformance

Target: `//tests/statemachine/cas-state:go_default_test`

Run with `bb remote test //tests/statemachine/cas-state:go_default_test --test_output=errors`.
The harness starts the digest-pinned enterprise v2.314.0 app and cache proxy,
using only disposable fixture-local authentication and storage. The runner must
provide `bb` on PATH; CLI subprocesses have an isolated HOME and no inherited
credentials or dispatch API key.

## Mutation and surface inventory

Inspected the released checkout at `~/buildbuddy-src`:

| Mutation | Component | Read surfaces checked after every successful batch | Result |
| --- | --- | --- | --- |
| REAPI `ContentAddressableStorage.BatchUpdateBlobs` (IDENTITY and ZSTD) | App | `FindMissingBlobs`, `BatchReadBlobs`, REAPI `GetTree`, `ByteStream.Read`, `cache.service.Cache.GetMetadata`, UI `BuildBuddyService.GetCacheMetadata/GetTree/GetTreeDirectorySizes` over both gRPC and HTTP, public `api.v1.ApiService.GetFile/GetFileRange` over both gRPC and HTTP, `bb download` | Reference-model assertions |
| Same | Cache proxy | `FindMissingBlobs`, `BatchReadBlobs`, REAPI `GetTree`, `ByteStream.Read`, `cache.service.Cache.GetMetadata`, `bb download` | Reference-model assertions |

Sources: `proto/remote_execution.proto`, `proto/cache.proto`,
`proto/cache_service.proto`, `proto/resource.proto`, `proto/buildbuddy_service.proto`,
`proto/api/v1/{service,file}.proto`,
`server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go`,
`server/remote_cache/directory_size/directory_size.go`,
`server/cache_server/cache_server.go`, `server/buildbuddy_server/buildbuddy_server.go`,
`server/http/protolet/protolet.go`, `server/libmain/libmain.go`,
`enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go`,
`enterprise/server/cmd/cache_proxy/cache_proxy.go`,
`enterprise/server/api/api_server.go`, and `cli/download/download.go`.

The proxy registers cache metadata, REAPI and ByteStream services; it does not
register BuildBuddyService or ApiService. Their projections are app-only.
`surfaces.proto` contains the exact field numbers needed to call those released
services, avoiding a product-source build or shared dependency change. UI tree
streaming uses `application/proto+prefixed`; its JSON route intentionally does
not implement streaming. Public HTTP GetFile returns raw file bytes.

## Reference model and transitions

Each component receives distinct content and a nonempty instance name. A map of
immutable digest-to-bytes values and Directory DAG nodes is compared with every
applicable surface after:

1. Initial batch of a file, a leaf Directory, and a root referencing that leaf
   twice under different paths.
2. Identical batch repetition.
3. Extension with a new file and root referencing the earlier root.
4. Identical extended-batch repetition.
5. Extension with a new file and root using ZSTD-encoded batch data.
6. Identical ZSTD batch repetition.

The model retains and rechecks earlier roots and bytes after extension. Existence
is checked before initial creation and after every successful write; batch
responses must contain exactly the expected digests with OK statuses. File ranges
exclude their first byte. Metadata checks logical size and nonzero physical size
and modification time, without assuming physical size equals logical size or
access times stay constant. Directory sizes include serialized Directory bytes
and count shared subtrees once per path; tree traversal compares directory sets.
The released size response uses hash-only keys, despite the proto comment's
hash/size description, so the model normalizes its keys for this projection.

CAS writes are immutable and deduplicated. This item owns BatchUpdateBlobs, not
ByteStream.Write or public DeleteFile: replacing bytes means writing a new digest,
and REAPI CAS has no update-in-place or delete RPC. No eviction, unsupported proxy
UI calls, or cross-component propagation assumptions are involved. Reads follow
synchronous successful mutation responses within a three-minute bounded context
per component; no sleeps or unbounded retries occur.

App image: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
Proxy image: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`.

## Validation

- [Final uncached run, including new ZSTD digests](https://app.buildbuddy.io/invocation/c5b01adf-1ade-4e0b-bc64-7ef1f5e74460): PASS, both components, 32.7 seconds.
- [Earlier surface-complete run](https://app.buildbuddy.io/invocation/c6509cc2-e1ce-48bf-9db4-568863dd6147): PASS, both components, before the extra ZSTD extension step.

No cross-surface state divergence was observed. No shared harness changes are needed.
