# Released CAS chunk state conformance

Run `bb remote test //tests/statemachine/cas-chunk-state/... --test_output=errors`.
Only the passing suite runs by default. Explicit failing regression targets are
`chunk_only_tree_repro` and `chunk_only_presence_repro`; use
`--test_tag_filters=bug --nocache_test_results --runs_per_test=2`.

Digest-pinned enterprise-v2.314.0 app and proxy containers start inside each test.
Exact image references/configs/logs are retained in fixture artifacts. The CLI is
the SHA256-verified released 5.0.492 binary fetched inside the remote test runner.
All component requests use fixture-local endpoints and disposable credentials.

The model contains immutable bytes plus mutable ordered chunk digests. Two
same-org API keys read the **same blob on both peers** after every splice and
replacement-chunk upload. Writers alternate app/proxy and credentials. Sequences
cover publication, idempotent repeat, replacement, repeat, second replacement,
and unsupported streaming registration on every peer/key. Each individual chunk
upload is checked through peer presence/batch/ByteStream/CLI/metadata and app UI
metadata and HTTP downloads; replacement uploads also recheck the prior full
model before updating the mapping. Both complete checks repeat after a 50 ms
stability interval. Mapping convergence is bounded to 10 seconds with 25 ms
polling and elapsed times logged (use `--test_arg=-test.v` to see successful logs).

| Mutating RPC | Surfaces checked | Result |
| --- | --- | --- |
| BatchUpdateBlobs (chunk/root upload) | Both peers/keys FindMissingBlobs, BatchReadBlobs status/bytes, ByteStream, bb download; Cache.GetMetadata size/stored bytes/timestamp bounds; app UI metadata gRPC/HTTP and /file/download, /file/view | Passing; replacement chunk upload preserves prior mapping |
| SpliceBlob | Both peers/keys SplitBlob count/order/function, chunk reconstruction/hash, presence, batch, ByteStream, CLI raw bytes, physical metadata; app UI gRPC/HTTP metadata and exact HTTP download/view bytes | Passing above presence threshold; default smaller-blob presence divergence isolated |
| SpliceBlob with Directory | Above plus CAS GetTree, UI GetTree gRPC and browser framed HTTP stream (including terminal status), UI GetTreeDirectorySizes gRPC/JSON, CLI typed Directory JSON | Passing with physical root; chunk-only GetTree divergence isolated |
| RegisterChunkMapping | Terminal Unimplemented, then repeat entire model check for each peer/key | Unsupported in this release, state remains unchanged |
| SplitBlob | Full ordered mapping plus returned FAST_CDC_2020 function after every update | Read-only manifest lookup in this release |
| GetChunkMapping | Terminal Unimplemented on every peer/key after each model check | Unsupported in this release |

The main suite uses 512 KiB average chunks and the default 2 MiB read fallback.
UNKNOWN is a valid unspecified function for content-verified arbitrary chunk
boundaries. The server labels returned manifests FAST_CDC_2020 even for these
accepted boundaries; the suite asserts the released response explicitly.

CAS has no public delete RPC and valid content cannot change under an existing
hash. Delete/re-create is therefore unavailable for this state machine. Internal
distributed-cache Delete is not a public authenticated CAS API; deleting fixture
storage would violate the valid-input/no-fault-injection scope. Streaming mapping
success cannot be exercised because the released server methods unconditionally
return Unimplemented; proxy methods forward them. Neither CLI nor web UI exposes
a separate chunk mapping command.

Metadata describes physical entries, not logical manifests. After warmed reads,
chunk-only app root metadata must be NotFound; proxy root metadata must succeed.
Every physical entry must have the exact digest size, positive stored bytes and
positive access/modify times no later than now; app Cache/UI/JSON stored size and
modify time agree. Access times can advance during reads and asynchronously, so
bounds are checked instead of requiring identical snapshots. Physically uploaded
Directory roots require successful metadata on both peers, with no permissive
success-or-NotFound branch.

## Confirmed findings

- [GetTree misses chunk-only Directory](app-cas-chunk-state-gettree.md): permanent lookup gap; no convergence in 10.04 seconds, reproduced twice with non-default 512 KiB chunk average.
- [FindMissingBlobs size gate](app-cas-chunk-state-presence.md): persistent missing state for a readable 2.72 MB splice, no convergence in ten seconds; reproduced twice with default config.

## Released-source surface enumeration

- `proto/remote_execution.proto`, `proto/cache.proto`, `proto/cache_service.proto`,
  `proto/buildbuddy_service.proto`: CAS/mapping/metadata/tree contracts.
- `server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go`:
  verified splice, manifest-only split, unsupported streaming, physical GetTree
  lookup, size-gated FindMissing fallback; `server/remote_cache/chunking/chunking.go`:
  thresholds and reconstruction; `server/remote_cache/byte_stream_server/byte_stream_server.go`:
  read fallback; capabilities server: supported functions and limits.
- `enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go`:
  forwarding; `enterprise/server/cmd/cache_proxy/cache_proxy.go`: registrations.
- `server/cache_server/cache_server.go`, `server/buildbuddy_server/buildbuddy_server.go`:
  physical metadata/UI delegation, tree adapters, /file/download and /file/view;
  `server/remote_cache/directory_size/directory_size.go`: cumulative size/count;
  `server/http/protolet/protolet.go`: browser stream framing and terminal status.
- `app/invocation/cache_requests_card.tsx`, `app/invocation/invocation_action_card.tsx`,
  `app/util/get_tree.ts`, `app/service/rpc_service.ts`: actual metadata, directory
  sizes, streaming tree, and download surfaces used by the UI.
- `cli/download/download.go`: raw and typed JSON download. Invocation logs,
  raw event JSON/proto, execution artifacts and ActionCache results expose other
  state (BES/execution/AC), not a CAS mapping mutation; the source handlers require
  invocation IDs and are not generic CAS reads.

## Round-2 validation

Final passing suite (180.44 seconds):
https://app.buildbuddy.io/invocation/4b3d2a40-5ce0-4e95-8afc-271554931eb6.
Ten mapping transitions converged on the first polling attempt, observed after
28.374626–32.687258 ms (includes the scheduled 25 ms polling interval and two
RPCs, so this is observation latency, not proof of propagation delay). All
complete post-convergence and delayed stability checks passed on both peers/keys.

Both isolated defects reproduced twice on independent fixtures using final test
sources: https://app.buildbuddy.io/invocation/3b45a649-b67f-46bb-8c6b-11c0f59559a9.
GetTree remained inconsistent for 10.010724–10.013619 seconds; default-config
presence remained inconsistent for 10.082831–10.094911 seconds. These are bounded
observation windows, with persistent lookup gaps explained by the released source;
no eventual recovery was observed. Both bug targets remain excluded by default.
No shared harness/dependency changes are needed; no public CAS delete or supported
streaming mapping mutation is available to extend these sequences further.
