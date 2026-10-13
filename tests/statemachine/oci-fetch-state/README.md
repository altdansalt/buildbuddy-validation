# OCI fetch-induced cache state

Released v2.314.0 source enumeration:

| RPC | Persistent mutation | Read surfaces covered |
| --- | --- | --- |
| OCIFetcher.FetchManifest | Manifest AC entry, auxiliary OCIManifestContent | Repeat FetchManifest, FetchManifestMetadata, GetActionResult on app/proxy; app GetCacheMetadata via gRPC and cookie-authenticated JSON protolet |
| OCIFetcher.FetchBlob | Blob CAS bytes, metadata CAS proto, AC record linking both; proxy also caches bytes locally | Repeat FetchBlob, FetchBlobMetadata; app/proxy GetActionResult, ByteStream.Read, CAS.FindMissingBlobs, CAS.BatchReadBlobs, Cache.GetMetadata; app GetCacheMetadata gRPC/JSON; bb download on both endpoints |
| OCIFetcher.FetchManifestMetadata | In-memory access proof only; no persistent cache write | Manifest digest/size/media type, AC absence before content fetch, metadata agreement after each fetch |
| OCIFetcher.FetchBlobMetadata | In-memory access proof only; no persistent cache write | Size/media type; AC and CAS absence before content fetch; agreement with persisted OCIBlobMetadata after each fetch |

Enumeration sources: `proto/oci_fetcher.proto`, `proto/ociregistry.proto`,
`proto/cache.proto`, `proto/cache_service.proto`,
`enterprise/server/oci/ocifetcher/ocifetcher.go`,
`enterprise/server/oci/ocicache/ocicache.go`,
`enterprise/server/ocifetcher_server_proxy/ocifetcher_server_proxy.go`,
`server/cache_server/cache_server.go`,
`server/buildbuddy_server/buildbuddy_server.go`,
`server/util/authutil/authutil.go`, `cli/download/download.go`.

The state model starts absent, preserves absence after metadata-only fetches,
and records the first content response. Each subsequent content/metadata/cache
read must match that model and the registry's digest-pinned descriptor. Two
fresh fixtures exercise app-first and proxy-first cold content fetches. Each
operation repeats, and both surfaces are compared after each content operation.
AC propagation polling is bounded to 10 seconds; all RPCs have a four-minute
sequence context. No fault injection, eviction, malformed requests, or artificial
cache writes/deletes are used. OCI RPCs expose neither update/delete nor a
platform selector: the selected linux/amd64 manifest is pinned explicitly and
its config blob must confirm that platform. These tests do not claim multiarch
index resolution coverage, which belongs to the OCI resolver rather than this
RPC service.

The app needs `ocifetcher.enabled=true`. OCI AC projections require trusted
internal identities. Both fixtures receive a disposable signing key, and the
test presents a correctly signed executor identity along with its disposable
local group API key. The test does not weaken AC authorization. CAS resources
are also downloaded through ordinary CLI clients with only the local API key.
Manifest bytes live inside AC auxiliary metadata, so there is no standalone CAS
resource to download for them. Cache proxy has no BuildBuddyService web API;
its local Cache.GetMetadata API covers the exposed metadata surface. The separate
optional OCI HTTP mirror service is not enabled by this fixture.

Fixtures and fetched manifest:

- App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
- Blob: manifest's config descriptor, sha256 `2fe44bd2522d833e060bd264885f443ed41ff9a528cb45e19eda55d1820bbf5d`, 3076 bytes.
- CLI: upstream source tag's pinned dependency, version 5.0.466, linux/x86_64,
  sha256 `df61a209cc45ecff9059bb04e01cc1dbc2b98a128715cada2a2a54bb4deba485`.
  Downloaded from its public GitHub release inside the remote test; checksum is
  verified before execution. CLI subprocesses receive only PATH and explicit
  fixture-local endpoint/key arguments, and run in temporary directories.

Run: `bb remote test //tests/statemachine/oci-fetch-state:go_default_test --test_output=errors`

No shared harness or dependency changes are required. Protobuf wire field
numbers are locally encoded to avoid building or importing BuildBuddy source.

Validation: expanded suite passed at
https://app.buildbuddy.io/invocation/2f97caf8-1148-4804-bbfc-aae29c12beff
(remote dispatch https://app.buildbuddy.io/invocation/ad7fc6c9-09a4-4813-b6a3-10ba666f3246).
Earlier content/cache/CLI coverage also passed at
https://app.buildbuddy.io/invocation/5b1affd5-c3d1-41a0-bdef-43f72ede0f81.
No confirmed divergence was observed. Preliminary failures were test setup
(missing trusted client identity) and a protobuf comparison including internal
memoized serialization size; both were corrected before these passing runs.
