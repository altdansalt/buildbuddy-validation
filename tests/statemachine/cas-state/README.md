# CAS state-machine conformance, round 2

Targets:
- `//tests/statemachine/cas-state:go_default_test`: passing conformance coverage.
- `//tests/statemachine/cas-state:delete_state_test`: isolated `bug` reproducer.

Use `bb remote test //tests/statemachine/cas-state/... --config=nobug --test_output=all --test_arg=-test.v --nocache_test_results`.
Explicit bug reproduction requires `--test_tag_filters=bug` without `--config=nobug`.
Tests start pinned released enterprise v2.314.0 containers inside the hosted runner.
CLI commands use fixture keys, an isolated HOME, and no inherited dispatch credentials.

## Mutations and checked projections

| Mutation | Surfaces checked after every step | Result |
| --- | --- | --- |
| App and proxy BatchUpdateBlobs, IDENTITY / ZSTD | Both components: FindMissingBlobs, BatchReadBlobs identity/compression negotiation, ByteStream full/ZSTD/range reads, cache GetMetadata, bb download bytes, Directory JSON and directory filesystem contents. App: UI GetCacheMetadata, GetTree, GetTreeDirectorySizes over gRPC and UI HTTP; GetFile and GetFileRange over public gRPC/HTTP; compressed GetFile over both transports; /file/download and /file/view; ZIP manifest via UI gRPC/JSON HTTP and extracted ZIP entry bytes via both frontend endpoints | PASS |
| App and proxy ByteStream.Write, IDENTITY / ZSTD | Same entire live-state projection matrix; preservation of all prior digests/roots | PASS |
| App DeleteFile, repeat delete, re-create via second same-org key | Every remaining live blob's app projections, deleted digest FindMissingBlobs, BatchReadBlobs both compressor negotiations, ByteStream, cache/UI metadata over gRPC and HTTP, public file/range gRPC and HTTP, frontend download/view, CLI failure; stable full-state recheck after re-create | PASS on app; non-default enable_cache_delete_api required |
| App DeleteFile after valid proxy write and warm read | App/proxy FindMissingBlobs plus app/proxy exact-byte ByteStream.Read, polled for 5 seconds | Confirmed persistent proxy read divergence; see findings/cacheproxy-cas-state-delete-stale.md |

## Source inventory and applicability

Re-inspected the released checkout's `proto/`, `server/`, `enterprise/`, `cli/`,
and `app/` code. `AUDIT.md` preserves the original audit and its clean probe.
Its frontend checks and same-model peer reads were incorporated with no perturbations.

Relevant source paths:
- `proto/{cache,resource,zip,buildbuddy_service,cache_service}.proto`, `proto/api/v1/{service,file}.proto`.
- `server/remote_cache/{content_addressable_storage_server,byte_stream_server,directory_size}`, `server/cache_server`, `server/buildbuddy_server`, `server/http/protolet`, `server/libmain`.
- `enterprise/server/{content_addressable_storage_server_proxy,byte_stream_server_proxy,api}`, `enterprise/server/cmd/cache_proxy`.
- `app/service/rpc_service.ts`, `app/util/get_tree.ts`, `app/invocation/{invocation_action_card,cache_requests_card}.tsx`, `app/target/target_artifacts_card.tsx`.
- `cli/download/download.go`.

Frontend raw invocation JSON/proto and build/run logs read invocation blobstore,
not CAS. They are not projections of these mutations. The bytestream frontend
mode does expose CAS and is checked, including ZIP-entry extraction. Invocation
artifact CLI mode likewise needs BES state and is not a CAS projection.
The proxy does not register app UI/public API services; app-only endpoints are
checked against the same reference model after proxy mutations.
QueryWriteStatus is explicitly Unimplemented in this release and not an oracle.

GetTree requests set page_size=1 on REAPI and UI gRPC/HTTP. The released server
ignores page-size hints, streams the complete tree and returns no continuation
token. Checks compare sorted directory **multisets**, including the shared leaf
under two paths, and reject duplicate directory-size response keys. This tests
the released pagination behavior, not token-based pagination that it lacks.

## Sequences, timing and assertions

Both writer directions use one model/instance per sequence and query both readers.
Two independently provisioned keys in the same organization participate in reads
and writes. Steps are initial creation, exact repeat, extension through the peer,
repeat of only that new batch, ZSTD extension, exact repeat, identity and ZSTD
ByteStream extensions. Extensions upload only new digests; preserved blobs are
checked before any repetition can rewrite them. Each step polls exact bytes on
both peers with a 10-second bound, then checks all projections, waits 100ms and
checks all projections again on both peers. Logs report observation upper bounds,
not precise propagation latency. The ZIP archive is deterministic and retained
through all steps. CLI extraction checks the exact recursive file-path/byte map.

CAS update means adding a new digest: bytes cannot validly change under an existing
SHA256 key. The app-only lifecycle creates three versions, deletes the original,
repeats deletion, and recreates the original with a compressed ByteStream write.
It checks every step and stable re-created state using a second same-org key.
The isolated proxy deletion reproducer captures the blocked convergence sequence
without hiding it in passing coverage.

Metadata checks size, physical storage, positive modification time, future bounds,
nondecreasing access/modification times across writes, unchanged modification times for untouched digests and read-only stable checks, and exact app UI modification
stamp agreement. Component storage/timestamps need not be equal across separate
backends. JSON numeric parse/type errors fail rather than defaulting to zero.
Absent frontend CAS requests return the release's explicit 400 Missing invocation_id
fallback error; absence is asserted rather than accepting arbitrary failures.
Public streaming GetFile returns the pooled client's specific sanitized Unavailable
missing-resource error (HTTP 404); HTTP GetFileRange returns 500 with its explicit
NotFound body. These release-specific error mappings still expose absence.
The app-only fixture waits up to 10 seconds for its self-gRPC metadata client to
reconnect, using a never-uploaded digest and accepting only the expected NotFound
as readiness. Unavailable transport errors are retried only during that readiness
phase; subsequent state checks assert exact outcomes.

## Images and results

App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`.

- Final targeted directory run, `--config=nobug`, uncached: https://app.buildbuddy.io/invocation/35dbf103-39cd-426e-93d0-68b6d24679ea ; PASS, including CLI Directory JSON, preservation/stability timestamps and app delete/re-create; 219.3s; write agreement observed within 7.080–48.855ms.
- App-only lifecycle isolation run: https://app.buildbuddy.io/invocation/a77ecd15-9c56-438e-a613-a506b2af7483 ; PASS.
- Expanded batch/ByteStream/ZIP suite: https://app.buildbuddy.io/invocation/c7a61df8-662f-4059-9759-184a79098b12 ; PASS; agreement observed within 8.055–51.826ms, full test 120.9s.
- Confirmed deletion divergence: https://app.buildbuddy.io/invocation/644b328c-956b-4bcf-af3c-95076fca6555 and https://app.buildbuddy.io/invocation/bcd15930-c0b5-45a1-8572-63cb8e89f7a3 ; persisted beyond 5.079s and 5.102s.

No shared changes, generated index edits, PRs or upstream reports.
