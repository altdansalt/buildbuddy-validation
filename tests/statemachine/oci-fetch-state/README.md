# OCI fetch state conformance, round 2

| RPC | State model | Surfaces checked | Result |
| --- | --- | --- | --- |
| FetchManifest | Pinned registry bytes → one AC auxiliary OCIManifestContent record; repeated valid fetches preserve content | FetchManifest and FetchManifestMetadata; app/proxy GetActionResult; app Cache.GetMetadata; app GetCacheMetadata gRPC/cookie JSON; `/file/download`, `/file/view`, ApiService.GetFile gRPC and HTTP using actioncache URLs; optional OCI mirror GET/HEAD | Content and app metadata agree; isolated repro confirms proxy native AC metadata missing |
| FetchBlob (streaming) | Config CAS bytes + metadata CAS proto + AC record with exactly one output of each path | FetchBlob and FetchBlobMetadata; app/proxy AC, ByteStream.Read, FindMissingBlobs, BatchReadBlobs (digest/status/content), Cache.GetMetadata for **both** CAS outputs; app metadata gRPC/JSON for AC and both outputs; both file handlers; ApiService.GetFile gRPC/HTTP, GetFileRange; bb download for both CAS outputs from both endpoints; mirror GET/HEAD | Passing coverage; proxy native AC metadata exception is isolated |
| FetchManifestMetadata | Access proof only; persistent model unchanged | Exact digest, size and media from an independent digest-verified registry GET; every client's AC and Cache.GetMetadata absence; app gRPC/JSON metadata absence and HTTP content errors; blob AC/CAS absence across every client | Pass |
| FetchBlobMetadata | Access proof only; existing manifest stays unchanged, blob state stays absent until content fetch | Exact size and pinned expected layer media; every client's AC, CAS presence/batch/ByteStream/Cache.GetMetadata absence; app gRPC/JSON metadata absence, both HTTP content errors, CLI failure on absent CAS; all existing manifest projections stay equal | Pass |

Every record receives an independent content assertion. A duplicate blob path
cannot bypass the metadata branch. Metadata CAS contents must equal the exact
size/media proto; both output digests are checked against their bytes. Manifest
content is compared to a registry GET whose SHA256 must equal the pinned release
digest, rather than trusting a first OCIFetcher response. Manifest media is fixed
to Docker schema 2; the blob metadata API uses the layer media default
`application/vnd.docker.image.rootfs.diff.tar.gzip`, even for the image config
blob (its descriptor media is a different concept).

Two cold fixtures run app-first and proxy-first sequences. Each has two API keys
in one org and four independent connections. Each valid content operation is
repeated from every connection, with a complete persistent projection snapshot
before the next content operation. The untouched blob remains absent after every
manifest operation; the existing manifest stays equal during every blob operation.
Metadata-only calls are followed by absence/preservation checks rather than
being credited as content mutations.

AC visibility polling tries immediately, accepts only NotFound while waiting,
and fails after ten seconds; unexpected errors fail immediately. The test logs
observed read visibility latency and compares AC again after 100ms without another
write. This measures RPC-visible state, including intended read-through, rather
than claiming unsolicited cache replication. In the passing expanded run
[def7658d](https://app.buildbuddy.io/invocation/def7658d-13c1-4ca3-882d-fbfdc0e2d169),
visibility checks were immediate successes, about 0.35–2.06ms, with stable later
AC results. The four-minute sequence context bounds all fixture-local RPCs.

The default coverage target stays green. The minimal
[proxy AC metadata finding](findings/cacheproxy-oci-fetch-state-ac-metadata.md)
is confirmed by two isolated remote reproductions. Proxy metadata stayed absent
for 10.002822732s and 10.001087934s. Source establishes a structural mismatch
between the transformed local AC key and native metadata lookup; the permanence
claim is based on that implementation, while observations are bounded to 10s.
The `bug` target retains the desired success assertion and fails until this
conformance gap is resolved.

## Released source enumeration and applicability

Inspected locally at v2.314.0:

- `proto/oci_fetcher.proto`, `proto/ociregistry.proto`, `proto/cache.proto`,
  `proto/cache_service.proto`, `proto/resource.proto`, `proto/buildbuddy_service.proto`,
  `proto/api/v1/{service,file}.proto`.
- `enterprise/server/oci/{ocifetcher,ocicache,ociregistry}/*.go`,
  `enterprise/server/ocifetcher_server_proxy/ocifetcher_server_proxy.go`,
  `enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go`,
  `enterprise/server/api/api_server.go`.
- `server/cache_server/cache_server.go`,
  `server/buildbuddy_server/buildbuddy_server.go`, `server/libmain/libmain.go`,
  `server/util/authutil/authutil.go`, `cli/download/download.go`.
- Frontend `app/service/rpc_service.ts` and `app/util/cache.tsx`: file downloads
  and views use bytestream/actioncache URLs plus selected-group request context;
  cache metadata uses GetCacheMetadata. These actual routes are exercised.

Raw invocation JSON/proto, build logs, execution profile streams, invocation
metadata, GetExecutionDownloads, and CLI invocation artifact listing do not
expose OCI fetch records: there is no invocation or execution ID in this model.
GetTree and tree-size APIs require Directory protos, whereas these CAS objects
are JSON/config and OCIBlobMetadata, so those would be invalid read inputs.
ByteStream.QueryWriteStatus concerns an upload resource name; OCI fetch exposes
none. CLI download supports CAS only, so manifest bytes inside AC have no CLI
CAS download surface. The proxy exposes Cache.GetMetadata but has no web
BuildBuddyService. The app's ApiService content endpoints are tested separately
from ByteStream because they are another public projection.

OCI fetch has no update or delete RPC and digest-addressed content is immutable.
Consequently create→update→delete→re-create cannot be expressed using valid OCI
fetch requests. These sequences use absent→metadata-only→create→repeat/repeat,
through both endpoints and both org keys. Artificially overwriting a pinned
manifest under the same digest would violate the valid-input restriction;
cache-internal deletes and cache-state mutation suites belong to other areas.
Tag-changing registry fixtures and admin-only `bypass_registry` remain outside
this digest-pinned suite. The latter requires an explicitly configured server
admin group; ordinary fixture org keys are not silently promoted. Interrupted
streams/eviction are omitted under this round's no-fault-injection restriction.
This suite does not claim multiarch resolver or private-registry credential
coverage.

## Configuration and exact images

Coverage enables `ocifetcher.enabled=true` and `ociregistry.enabled=true` on the
app, plus disposable `app.client_identity` signing keys on app/proxy. Correctly
signed executor identities authorize restricted OCI AC reads. HTTP content
handlers use the actual cookie-selected org or a disposable fixture API key;
the app supplies its trusted identity on internal calls. Mirror checks run only
after canonical cache assertions, use explicit manifest Accept headers and
identity HTTP encoding, and verify digest headers, exact lengths and bytes.
They may read through to the public registry; they are not used to prove the
absence of fallback. The isolated metadata repro needs no mirror configuration.

For absent CAS `/file/*` calls, the released handler tries invocation-artifact
fallback and returns 400 (missing invocation ID); absent AC returns 404. Both
error paths are asserted exactly, while GetMetadata/ByteStream/CAS return
NotFound. They do not get mistaken for successful empty downloads.

- App/fetched manifest: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
- Config blob: descriptor sha256 `2fe44bd2522d833e060bd264885f443ed41ff9a528cb45e19eda55d1820bbf5d`, 3076 bytes; confirms linux/amd64.
- CLI 5.0.466 linux/x86_64: `df61a209cc45ecff9059bb04e01cc1dbc2b98a128715cada2a2a54bb4deba485`, downloaded and hash-verified inside the test. Subprocess environment is PATH only, with explicit fixture endpoints/keys.

Run coverage:
`bb remote test //tests/statemachine/oci-fetch-state/... --nocache_test_results --test_output=all`

Run known divergence:
`bb remote test //tests/statemachine/oci-fetch-state:proxy_ac_metadata_repro --test_tag_filters=bug --nocache_test_results --test_output=all`

No shared harness/dependency changes or PR are needed. Audit perturbations were
not retained. All changes and the finding are confined to this directory.
