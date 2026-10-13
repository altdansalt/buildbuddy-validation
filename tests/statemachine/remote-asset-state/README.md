# Remote Asset state conformance

Released images: app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`, proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906` (enterprise-v2.314.0; harness defaults).

## Mutation and read-surface inventory

| RPC | Released implementation | Read surfaces asserted |
| --- | --- | --- |
| Fetch.FetchBlob | Fetch HTTP bytes into CAS; checksum.sri may reuse existing CAS content. Mutable URLs are downloaded again; oldest_content_accepted disables coalescing. | Both app and proxy: FetchBlob repeat, CAS.FindMissingBlobs, CAS.BatchReadBlobs, ByteStream.Read, cache.service.Cache.GetMetadata, bb download (runner CLI required). App: BuildBuddyService.GetCacheMetadata over gRPC and cookie-authenticated JSON UI endpoint; /file/download; api.v1.GetFile over streaming gRPC and HTTP; api.v1.GetFileRange over gRPC and HTTP. |
| Fetch.FetchDirectory | Unimplemented | Excluded from positive assertions. |
| Push.PushBlob | Unimplemented | Excluded from positive assertions. |
| Push.PushDirectory | Unimplemented | Excluded from positive assertions. |

Source inventory (v2.314.0): `server/remote_asset/fetch_server/fetch_server.go`, `server/remote_asset/push_server/push_server.go`, `proto/cache.proto`, `proto/cache_service.proto`, `proto/resource.proto`, `proto/buildbuddy_service.proto`, `proto/api/v1/file.proto`, `enterprise/server/api/api_server.go`, `server/buildbuddy_server/buildbuddy_server.go`, `server/libmain/libmain.go`, `enterprise/server/cmd/cache_proxy/cache_proxy.go`, and `cli/download/download.go`. Proxy registers Fetch, CAS, ByteStream and Cache metadata, but no native UI or api.v1 server. No supported association deletion RPC exists.

The origin is an in-test HTTP server on the remote runner. A literal fixture bridge gateway address and an explicit private-address allowlist permit that fixture-local origin. All content reads target fixture components; bytestream URIs use the app's Docker alias. No shared deployment is addressed.

Each app/proxy sequence creates content via a checksum-qualified cold fetch, repeats it, changes origin bytes, verifies the old immutable association, fetches the new content with freshness constraints, repeats, and checks checksum hits and ordinary mutable fetch. A reference map retains every returned digest and its expected bytes. Each operation rechecks every retained version across the surfaces that expose it. App-origin fetches become readable through both components; cross-component presence uses bounded polling; byte and metadata assertions then check exact content/size. Origin counters distinguish checksum cache hits from fresh HTTP fetches. CAS is immutable: origin updates must preserve older digests, rather than deleting them.

The test uses call-local protobuf wire encoding for BuildBuddy-specific API messages because the shared dependency scaffold has no generated bindings. Field numbers come from the released protos above. The runner must provide bb; no shared harness changes are needed.

## Documented proxy-local population

`fetch_server.getByteStreamClient` selects the local ByteStream client on the proxy. The released `TestFetchBlob_CacheProxy` explicitly states that fetched assets must not propagate upstream, to reduce cross-zone egress. This is a documented semantic difference, not a confirmed defect: proxy-origin fetches are checked against proxy CAS, ByteStream, metadata and CLI, and app CAS is asserted missing. The app's streaming GetFile and full-file HTTP readers can read a proxy-addressed URI and are checked too. App metadata and GetFileRange read the app cache directly, so are only positive read surfaces for app-populated data. Cacheproxy has no native BuildBuddyService or api.v1 routes in this configuration.

No Remote Asset delete/update-association RPC is implemented. Origin changes plus freshness-constrained fetches exercise supported updates, and the old digest remains readable locally. FetchDirectory and both Push RPCs are intentionally excluded as requested.
