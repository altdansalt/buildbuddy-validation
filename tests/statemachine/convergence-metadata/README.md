# Released resource metadata convergence

Green target: `//tests/statemachine/convergence-metadata:go_default_test`.
Known-divergence target: `:cli_namespace_repro` (`bug` tag; run explicitly with
`--test_tag_filters=bug --nocache_test_results`). Audit probes are retained with
all original, unperturbed expectations in `TestAuditMissingSurfaces`.

| Mutating RPC | Asserted surfaces | Sequences |
| --- | --- | --- |
| BatchUpdateBlobs | app/proxy identity and zstd ByteStream reads, identity/compressed BatchReadBlobs, FindMissingBlobs, Cache.GetMetadata; app UI metadata gRPC, JSON HTTP and frontend framed HTTP; HTTP file/log bytes; app/proxy bb download | create and repeat via both endpoints; directory-shaped objects |
| ByteStream.Write identity/zstd | same CAS surfaces; directory objects additionally app/proxy REAPI streamed GetTree, UI streamed GetTree over gRPC and actual length-prefixed HTTP, UI GetTreeDirectorySizes over gRPC/JSON/framed HTTP; CLI Directory JSON and recursive extraction | both endpoints, repeated writes; changing nested directories and alternating writer/key |
| UpdateActionResult | app/proxy default and inline stdout/stderr/full result, inline output file bytes, exit code, file/directory/symlink and execution metadata; app HTTP AC protobuf; app exact stored size and advancing modification time over Cache/UI metadata; proxy request-hashed pointer metadata and backing result CAS metadata | create → replace → replace → identical repeat; writers alternate app/proxy, two keys in one org |
| SpliceBlob | app/proxy existence before read-through, ordered SplitBlob, identity/zstd ByteStream and HTTP reconstruction; CLI isolated known-divergence target | app/proxy creation and repeat, 5 MiB valid manifest |

Every primary mutation is followed by reference-value checks. Content is sampled
three times with 100 ms intervals after convergence. AC convergence polling uses
10 seconds / 100 ms and logs elapsed time; existence polling precedes content
reads in the shared content checker. Elapsed times are observation upper bounds
including RPC and polling scheduling, not claims of an actual inconsistent
interval. The original CAS path logs proxy metadata before content read-through,
so local absence is visible instead of silently interpreted as proactive replica
convergence. Proxy reads are demand-filled; local Cache.GetMetadata is not a
remote read. AC results and shared output bytes are deterministic; worker
metadata is supplied to avoid server-generated host IDs.

The fixture uses default config and digest-pinned enterprise-v2.314.0:

- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

## Source inventory and representation boundaries

- `proto/{remote_execution,cache,resource,cache_service,buildbuddy_service}.proto`:
  request identities, mutation/read declarations, metadata and tree fields.
- `server/{cache_server,buildbuddy_server}`: local metadata reads, UI delegation,
  `/file/download` CAS and actioncache decoding.
- `app/service/rpc_service.ts`: text, arraybuffer and streaming log/raw downloads
  all use `/file/download`; frontend RPCs use `application/proto+prefixed`.
  `server/http/protolet/protolet.go` defines message framing and terminal status.
- `app/invocation/{invocation_action_card,cache_requests_card}.tsx` and
  `app/util/get_tree.ts`: file/AC downloads, streamed tree and metadata calls.
  Raw JSON is rendered client-side from downloaded protos, not a separate RPC.
- `server/remote_cache/directory_size`: cumulative sizes include serialized
  directories and files; `digest` response strings are hashes in this release.
- `enterprise/server/action_cache_server_proxy`: default salt
  `actioncache-170401`; request-hashed keys hold ResourceName pointers to local
  CAS results. Tests check pointer metadata using that identity, not action keys.
- `enterprise/server/backends/pebble_cache`: default auto-zstd threshold is 100
  logical bytes. Access-time updates are batched; no false requirement for exact
  access-time advancement. Metadata is storage-specific, not equal across peers.
- `server/remote_cache/chunking` and CAS server/proxy: whole spliced blobs expose
  manifests and reconstruction, not materialized whole-blob metadata records.
- `cli/download/download.go`: byte downloads, Directory JSON and recursive tree
  extraction; namespace parsing is the isolated divergence candidate.

There is no public cache-resource delete RPC. Delete/recreate cannot be added
without eviction, internal cache access, or fault injection. QueryWriteStatus,
GetChunkMapping and RegisterChunkMapping return Unimplemented in this release.
A directory RPC is applicable only to Directory protos, not arbitrary CAS bytes;
invocation-specific artifact/BES/execution endpoints require their own invocation
state and do not project these anonymous cache writes. Zip member extraction
requires zip-shaped data and is outside this metadata/directory fixture.
BatchReadBlobs excludes 5 MiB splice objects because the default request limit is
4,000,000 bytes. CLI is required, never skipped; the installed runner binary is
identified by SHA256 in the isolated repro. A shared pinned CLI runfile remains
a desirable coordinator change, since the runner CLI is not the enterprise image.

No shared files, dependencies, harness configuration or deployment are modified.

## Round 2 results

Green expanded suite (uncached; latest rerun https://app.buildbuddy.io/invocation/0e12a5f8-b96b-4434-ad86-adf79499d073, CLI repro third run https://app.buildbuddy.io/invocation/211c7202-5873-4c30-ba38-43896397e7e5; earlier):
https://app.buildbuddy.io/invocation/989c7e7c-3759-441c-b3c6-711079242a6f.
Existence observation upper bounds: 202.930–206.409 ms. Full AC content/metadata
observation upper bounds: 312.718–314.337 ms; all subsequent stability samples
passed. Polling cadence contributes to these numbers; no actual transient
incorrect content was observed.

Confirmed CLI namespace divergence, persisting through 12.4–12.5 seconds on
both endpoints in two isolated runs:
https://app.buildbuddy.io/invocation/f2e18b72-8a1e-43e4-a7de-8b29ea032040 and
https://app.buildbuddy.io/invocation/b5c4e669-8885-47dc-b804-4f419522ae15.
See `app-convergence-metadata-cli-namespaced-splice.md`. Its `bug` target is
excluded by default. All other asserted surfaces agree under default config.
