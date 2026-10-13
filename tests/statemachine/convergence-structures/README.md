# Directory and chunk state convergence, round 2

Released enterprise app and cache proxy only; all traffic addresses disposable
fixture-local containers. Main target:
`//tests/statemachine/convergence-structures:go_default_test`.

| Mutating RPC | Surfaces checked after every step | Result |
| --- | --- | --- |
| BatchUpdateBlobs | App/proxy ByteStream full, ranged and zstd reads; identity/zstd-negotiated BatchRead bytes/status/digest/cardinality; exact FindMissing; SplitBlob mapping absence or ordered reconstruction; Cache.GetMetadata; app BuildBuddyService.GetCacheMetadata gRPC/HTTP; ApiService.GetFile/GetFileRange gRPC/REST; HTTP file download/view; CLI raw bytes and Directory JSON/extraction | Reference-model equality and stable repeated samples; ZIP content-offset metadata boundary documented |
| ByteStream.Write | Same surfaces as BatchUpdateBlobs, including historical state | Reference-model equality and stable repeated samples |
| SpliceBlob | All preceding surfaces plus ordered SplitBlob reconstruction; all published directory/UI/CLI roots continue to reference exact content | Confirmed app limited-read/range/metadata representation gap; known boundaries are asserted in green coverage; desired behavior in isolated bug target |
| RegisterChunkMapping | Explicit Unimplemented on app/proxy, followed immediately by complete model checks | Released supported-surface boundary; no successful mutation |
| ApiService.DeleteFile | App/proxy read/presence/batch/ranged/zstd/metadata/CLI; app public file/range APIs over gRPC/REST, UI metadata/download/view; repeated delete then opposite-endpoint re-create and repeated publication | Confirmed stale proxy read versus remote presence divergence; opt-in API; green lifecycle pins boundary and continues re-creation |

For every published Directory, app/proxy CAS.GetTree, UI BuildBuddyService.GetTree
over gRPC and framed HTTP, GetTreeDirectorySizes over gRPC and JSON HTTP, CLI
Directory JSON and recursive extraction are checked against the model after
every subsequent mutation. This includes standalone leaf directories before a
parent root exists. Directory multiplicity is checked exactly against reachable DAG paths; unexpected duplicate directories and duplicate size entries fail. Size responses
require exact membership, with recursive sizes/counts counting repeated DAG
edges. CLI checks exact file membership, bytes and executable permissions.

A deterministic two-entry ZIP is uploaded through BatchUpdateBlobs. After every
step, gRPC/HTTP GetZipManifest names, cardinality, size/offset/checksum fields and
HTTP download/view ZIP-entry bytes are checked. Ordinary bytes, logs and raw
JSON views use the same frontend `/file/download` and `/file/view` paths; the
suite compares raw response bytes, rather than browser presentation.

The main structural sequence uses two local API keys in one org, alternating
publication endpoint and upload RPC. Three chunks → splice → repeated splice →
leaf → five root revisions (create, two updates, remove path, re-create path),
with repeated identical publication through the other upload API. Historical
immutable roots stay readable. Removing a path is not called blob deletion.
Literal deletion is separately exercised by the lifecycle and isolated repro.
Deleted-resource public GetFile sanitizes missing reads to Unavailable. HTTP UI artifact fallback uses a real completed fixture-local invocation; with the fixture disk blobstore, a missing fallback artifact is sanitized to HTTP 500/Internal. These negative transport boundaries are asserted and are not presented as stale-content divergences.

The proxy exposes no ApiService DeleteFile service; deletion is through the app,
with publications/re-creations through either endpoint.

Convergence checks poll every 100 ms with a two-minute deadline. Each successful
complete sample logs elapsed time and is followed by two complete samples,
separated by 100 ms; a later mismatch fails immediately. Logged convergence is
an observed upper bound including all read/CLI sampling time, not precise
replication latency. The last substantive mismatch is retained if a final RPC
hits the deadline. HTTP clients and subprocesses have bounded contexts/timeouts.
CLI availability is mandatory. Read-only CLI subprocess concurrency is bounded at four to keep the expanded per-step matrix practical on CI. CLI uses only disposable HOME/PATH and the local
fixture key; the dispatch credential is never forwarded. Runner-installed `bb`
is not version-pinned, a remaining reproducibility limitation.

## Source inventory

Inspected locally at v2.314.0:
- `proto/remote_execution.proto`, `cache.proto`, `cache_service.proto`,
  `resource.proto`, `zip.proto`, `api/v1/file.proto`, `api/v1/service.proto`.
- `server/remote_cache/content_addressable_storage_server`,
  `byte_stream_server`, `chunking`, `directory_size`, `byte_stream_client`;
  `server/cache_server`, `server/buildbuddy_server`, `server/http/protolet`,
  and `server/libmain` service and HTTP registrations.
- `enterprise/server/content_addressable_storage_server_proxy`,
  `byte_stream_server_proxy`, `api/api_server.go`, `cmd/cache_proxy`.
- `app/service/rpc_service.ts` (file/view/download, ZIP entry transport),
  `app/invocation/invocation_action_card.tsx` (trees/sizes),
  `cache_requests_card.tsx` (metadata), and `cli/download/download.go`
  (raw blobs, Directory JSON, extraction).

QueryWriteStatus and GetChunkMapping/RegisterChunkMapping are explicitly
Unimplemented in the released source and asserted as boundaries. Flat uploaded
blobs have no explicit SplitBlob manifest and return NotFound; splice mappings
require exact order and byte reconstruction. Read warming is observable: the
isolated splice repro demonstrates warmed proxy limited reads succeeding while
app limited reads still fail. This suite makes no org-isolation claim and does
not exercise unrelated invocation, execution, action-cache or BES mutations.

## Findings and config

- [Warmed proxy deletion](FINDING-delete-warmed-blob.md): no convergence after
  30.0058 s and 30.1225 s; requires non-default `--enable_cache_delete_api=true`.
- [ZIP content offset](FINDING-zip-content-offset.md): permanent zero-valued documented content offset, default config; entry downloads still work.
- [Splice range/metadata](FINDING-splice-range-metadata.md): no convergence after
  10.0104 s and 10.1771 s; permanent representation gap absent flat re-upload.

Bug targets are tagged `bug` and excluded from normal CI. Run explicitly with
`--test_tag_filters=bug --nocache_test_results`. Findings contain both independent
remote invocation links, source explanations and precise persistence limits.

Structural config enables directory sizes, sets 256 KiB average chunks and a
1 MiB chunk-read threshold, and enables proxy CDC. The splice repro lowers the
app chunk-read threshold and enables proxy CDC so small CI-friendly input
exercises manifest-only reads. The deletion lifecycle enables only the public
delete API beyond harness defaults. Valid SHA256 inputs; UNKNOWN splice chunking
function permits opaque boundaries. No fault injection or component source build.

Exact pinned images:
- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

Only this directory is changed. No PR, shared harness changes or generated index
updates are requested. The audit HTTP probes were incorporated into per-step
checks with original expectations restored; no perturbation tests are retained.
