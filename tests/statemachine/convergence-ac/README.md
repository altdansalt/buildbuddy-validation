# Action-result and referenced-blob convergence

Target: `//tests/statemachine/convergence-ac:go_default_test`.

| Mutation | Surfaces checked | Sequence |
| --- | --- | --- |
| UpdateActionResult | App/proxy GetActionResult, all eight inline combinations, mutation response, app HTTP `/file/view` and `/file/download`, streaming gRPC/HTTP API GetFile AC bytes, app HTTP/gRPC GetCacheMetadata | Absent, six revisions, identical repeats, alternating app/proxy writers and two same-org keys; exit codes, executable files, raw/digest stdout/stderr, output Tree, symlinks, caller-supplied ExecutionMetadata (all nine timestamps and auxiliary metadata); retained old CAS blobs |
| BatchUpdateBlobs | App/proxy FindMissingBlobs, complete unique BatchRead digest sets and compressor/status/size, identity/zstd ByteStream reads and nonempty ranges, Cache.GetMetadata, app HTTP/gRPC GetCacheMetadata, HTTP view/download, streaming gRPC/HTTP API GetFile (identity/zstd), gRPC/HTTP GetFileRange, raw `bb download` | Every initial Action/Command/Directory and revision blob checked immediately; repeated identical writes through opposite endpoint |
| ByteStream.Write | Same CAS surfaces; committed-size mutation response | Identity/zstd writes, alternating app/proxy and keys |
| Directory uploads through both transports | Above blob surfaces, app/proxy REAPI GetTree, app BuildBuddyService.GetTree gRPC and framed HTTP stream including terminal status, UI HTTP and gRPC GetTreeDirectorySizes, recursive `bb download` with extracted byte oracle | Child and nested root revisions; exact Directory set, no duplicates; cumulative sizes/counts |
| API DeleteFile (AC) | All app/proxy GetActionResult variants and keys, HTTP view/download and streaming API GetFile absence, app HTTP/gRPC metadata absence; subsequent re-creation checked through AC read/download surfaces | Create → update → update → delete → re-create; app flag `--enable_cache_delete_api=true` explicitly required |

AC polling has a 15-second bound, then three complete spaced read sweeps
(500 ms between sweeps). CAS writes are checked synchronously after completion,
then every blob surface is checked twice more with 250 ms spacing. Retained
blobs are checked after every AC replacement. Logs report first full-sweep
observation durations; these are upper bounds on convergence, including serial
RPC/HTTP/CLI work, rather than estimates of network propagation alone.

Metadata checks include digest size, stored size, modification time, access time
and agreement between the app's JSON/gRPC projections. CAS stored size must
match the identity bytes or observed compressed representation. App and proxy
physical metadata timestamps need not match: local proxy population is a
separate physical write. Proxy AC entries use salted request-variant keys/CAS
pointers, so metadata at the original AC digest is not a projection of that
entry. ExecutionMetadata is preserved and compared in full; no provenance
submessage is stripped.

## Released source inventory and applicability

- `proto/remote_execution.proto`, `server/remote_cache/action_cache_server/`,
  `enterprise/server/action_cache_server_proxy/`: public AC Get/Update,
  inlining, output validation, remote validation of cached variants.
- `proto/cache.proto`, `proto/cache_service.proto`, `proto/resource.proto`,
  `server/cache_server/`, `server/buildbuddy_server/buildbuddy_server.go`:
  metadata, trees, cumulative directory sizes and file handlers.
- `proto/api/v1/file.proto`, `proto/api/v1/service.proto`,
  `enterprise/server/api/api_server.go`, `server/libmain/libmain.go`:
  streaming GetFile, bounded GetFileRange, opt-in DeleteFile. DeleteFile is
  available on the app, not the cache proxy; there is no REAPI AC delete RPC.
  GetFile also accepts actioncache URIs via the pooled byte-stream client,
  so its HTTP/gRPC AC bytes and post-delete absence are checked after each
  AC mutation alongside the UI file endpoint. GetFileRange accepts CAS only.
  For missing AC data the pooled file client sanitizes the exhausted reads to
  an exact Unavailable error; live AC RPCs must independently return NotFound,
  the HTTP file endpoints must return 404, and metadata must be absent.
- `app/invocation/invocation_action_card.tsx`, `app/service/rpc_service.ts`,
  `app/service/length_prefixed.ts`: Action/Command/Directory/Tree decoding,
  stdout/stderr text views, output downloads, directory sizes; streaming uses
  `application/proto+prefixed`, not JSON protolet requests.
- `cli/download/download.go`: raw/typed CAS downloads and recursive Directory
  extraction. The CLI does not expose AC GetActionResult. Invocation-artifact
  downloads require BES-created invocation state and do not project AC state.
- `server/remote_cache/byte_stream_server/byte_stream_server.go`: released
  QueryWriteStatus explicitly returns Unimplemented; it is not a successful
  write-completion oracle. The canonical empty digest is virtual and does not
  create physical metadata; fixtures use a nonempty valid input Directory.

Invocation raw-JSON/binproto, buildlog/runlog, execution lookup/profile/stream,
scorecard/history and snapshot APIs are projections of BES/execution state,
not standalone AC updates. These sequences create no invocation or execution,
so calling those APIs would not be a valid read of this reference model.
Zip entry extraction requires a zip object; these model blobs are not zips.

Images are pinned released enterprise-v2.314.0 defaults:

- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

Typed CLI `--type=Action`, `--type=Command`, and `--type=Directory` JSON is
decoded and compared to the reference proto. Recursive download JSON and
extracted bytes are both checked.

All service requests and CLI targets are fixture-local. Keys are disposable
local app keys; no dispatch key is passed into the tests. AUDIT.md preserves the
round-1 audit; all expectation perturbations were restored before importing it.

## Round 2 validation

The full coverage suite passed with all five tests on
[d2ded9f6-b0c3-40e0-a389-fade8cfd7a93](https://app.buildbuddy.io/invocation/d2ded9f6-b0c3-40e0-a389-fade8cfd7a93)
(381 seconds). AC replacement observation bounds were 37.46–50.25 ms;
lifecycle writes 132.45–136.83 ms; deletion 21.18 ms; complete blob sweeps
421.76–617.09 ms including CLI work. Full execution metadata sweeps took
121.43–127.80 ms. These passing observations had no first-sweep mismatches;
all spaced stability sweeps passed. Earlier strengthened coverage also passed
[f6ca43e3-0895-4441-8a72-c2b1c5fa6a04](https://app.buildbuddy.io/invocation/f6ca43e3-0895-4441-8a72-c2b1c5fa6a04).

`compressed_range_repro` is a separate `bug`-tagged target for the documented
compressed-URI GetFileRange contract. The default suite excludes it. Run it
explicitly with `--test_tag_filters=bug --nocache_test_results`.

Confirmed divergence: documented compressed GetFileRange fails with gRPC
FailedPrecondition / HTTP 500 for an available zstd-stored CAS blob. Two independent expanded reproductions each rejected all 61 attempts across
more than 15 seconds after both BatchUpdateBlobs and ByteStream.Write, under
default configuration. See
[the finding](findings-app-convergence-ac-compressed-range.md) for controls,
source explanation, pinned image and invocation links. This is a persistent
API rejection, not a measured transient app/proxy propagation mismatch.

Final complete suite, including AC API GetFile projections and their released
missing-resource status, passed:
https://app.buildbuddy.io/invocation/a9dcc0e4-5b8d-4f34-ab5c-966bdde5925b
(390.5 seconds; bug target excluded by the default configuration).
