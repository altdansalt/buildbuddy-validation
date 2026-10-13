# Streamed content and deletion convergence

Pinned v2.314.0 images: app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`, proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`. Actual image inspection artifacts are preserved by the fixture. All component traffic is test-local; the dispatch key never enters fixtures.

| Mutating RPC | Surfaces compared after each step | Result |
| --- | --- | --- |
| App/proxy ByteStream.Write, identity/zstd, create/repeat/alternate representation | Both endpoints: ByteStream full/prefix/EOF ranges identity/zstd, CAS BatchReadBlobs identity/zstd negotiation and exact digest/status/count/content, FindMissingBlobs, direct cache.service.Cache.GetMetadata; app: GetFile streaming gRPC/REST identity/zstd, GetFileRange gRPC/REST identity, UI file/download and file/view identity/zstd, UI GetCacheMetadata JSON/framed HTTP and direct gRPC; bb download identity/zstd through both endpoints | Green; each whole-model sweep repeated after 150 ms for stability; two API keys in same org |
| App/proxy ByteStream.Write of Directory protobufs, new versions and repeats | All above plus both CAS GetTree streams, BuildBuddyService.GetTree gRPC and UI framed HTTP stream, GetTreeDirectorySizes JSON/framed HTTP/direct gRPC, bb download --type=Directory JSON through app/proxy | Green; all prior directory versions checked after every write and on a second stability sweep |
| Proxy ByteStream.Write zstd (isolated contract repro) | Positive compressed full-file GetFile control; identity and zstd GetFileRange gRPC/REST | Permanent compressed range rejection; bug-tagged |
| App api.v1.DeleteFile after app/proxy writes and warmed reads | Both endpoints: ByteStream identity/zstd, CAS BatchReadBlobs and FindMissingBlobs, direct metadata, bb download; app: UI metadata JSON/direct gRPC, GetFile/GetFileRange gRPC/REST identity/zstd, file/download and file/view; re-create restores every present-state surface | Persistent proxy disagreement >30 seconds; opt-in delete API; bug-tagged |

The blob model is immutable digest → logical bytes: “updates” create new digests, and every prior entry remains readable. The model requests its stored hash rather than re-hashing to select a different object. Checks require exact content, logical digest size, returned digest identity, response count, and status. Directory trees use valid symlinks without requiring missing child blobs. CLI Directory JSON is decoded and compared as a protobuf, not substring-matched. There is no selected-snapshot-only checking. QueryWriteStatus is explicitly checked as Unimplemented on both released endpoints after writes; the source intentionally implements no resumable status state.

The normal suite logs convergence upper bounds (elapsed full validation sweep, not an inferred exact propagation instant) and repeats all surfaces after a 150 ms hold. Blob polling is capped at 30 seconds and preserves the first useful mismatch even if a later call hits DeadlineExceeded. Model traversal is sorted. Directory operations are synchronous, with bounded test context and two complete sweeps; no transient mismatch was observed. Deletion polls the entire missing-state projection for 30 seconds and reports all disagreeing surfaces, then continues to re-create and delete again. Permanent divergence assertions live in isolated bug targets so CI stays green.

Metadata checks include logical size, positive stored size/access/modify timestamps, plausible upper timestamp bounds, and agreement of stored size/modify timestamp across app UI/direct APIs. Access timestamps may advance on reads; framed UI access cannot precede the earlier JSON read. Proxy timestamps/storage are independent and are validated locally rather than incorrectly requiring identical physical storage/timestamps on both containers. CLI version is runner-provided and is not pinned; missing CLI fails the test.

## Source-to-surface inventory (released source)

- `proto/api/v1/file.proto`, `enterprise/server/api/api_server.go`: GetFile, GetFileRange, DeleteFile and REST mapping. DeleteFile needs `--enable_cache_delete_api=true`; no proxy delete RPC is registered.
- `server/remote_cache/byte_stream_server/byte_stream_server.go`, `enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go`: streaming writes/reads, repeated-write short circuits, logical ranges, zstd transcoding, local proxy hits, QueryWriteStatus restriction.
- `proto/cache.proto`, `proto/resource.proto`, `proto/cache_service.proto`, `server/buildbuddy_server/buildbuddy_server.go`, `server/remote_cache/directory_size/directory_size.go`: metadata, trees, cumulative sizes, and download/view handlers. Cumulative-size output keys are hashes in the released implementation, despite an older proto comment saying hash/size.
- `server/libmain/libmain.go`, `enterprise/server/cmd/cache_proxy/cache_proxy.go`: registered public services; app-only APIs are checked at app, cache metadata at both endpoints.
- `app/service/rpc_service.ts`: UI download URLs and application/proto+prefixed transport. `app/util/get_tree.ts`: streaming GetTree. `app/invocation/cache_requests_card.tsx`: renders metadata size/access/modify. `app/invocation/invocation_action_card.tsx`: stdout/stderr, raw cached protobufs, directory content, and text previews all fetch the same bytestream download endpoint; raw JSON is a frontend decode, not an independent server cache endpoint.
- `cli/download/download.go`, `cli/util/download/download.go`: digest download and typed Directory JSON, which delegate to ByteStream. Invocation build/run logs, raw BES JSON, and artifact listings expose BES/invocation state, not mutations of unattached CAS blobs, and are outside this streamed-cache item. Scorecard/usage attribution likewise needs invocation state; these writes carry no invocation ID.

UI missing-file behavior is asymmetric by design: GetFile gRPC uses pooled-client Unavailable for failed retrieval, REST returns 404, and download/view attempt invocation blobstore fallback after CAS absence. Without an attached invocation the final UI error is 400 Missing invocation_id. These exact missing-state forms are checked after successful authenticated present-state controls; unrelated errors do not count as absence.

## Run

`bb remote test //tests/statemachine/convergence-streams:go_default_test --nocache_test_results --test_arg=-test.v --test_output=all`

`bb remote test //tests/statemachine/convergence-streams:compressed_range_bug //tests/statemachine/convergence-streams:delete_convergence_bug --test_tag_filters=bug --nocache_test_results --test_output=errors`

See the two finding files for reproductions and timings. AUDIT.md is the unchanged historical round-1 audit from local commit 335441d; its old scope exclusions do not describe this round-2 suite. Audit expectation perturbations were never imported into the production tests. Findings remain here under the task's directory-only ownership rule; no INDEX or shared dependencies were edited.

## Round-2 evidence

Green expanded suite: https://app.buildbuddy.io/invocation/f3093070-2c71-4243-a439-35fe6765e2bb. Blob sweep convergence upper bounds 0.918–1.108 s; two-pass tree sweeps 2.642–5.221 s. No transient mismatch was observed in these successful checks; sweep duration is an upper bound including verification overhead.

Compressed-range reproductions: https://app.buildbuddy.io/invocation/1ec15f22-09c0-4419-bc39-cf55fe8ce6c9 and https://app.buildbuddy.io/invocation/29045d85-fceb-4d11-aa01-67e88ff7dda8.

Deletion reproductions: the same two invocations; corrected expanded surface probe https://app.buildbuddy.io/invocation/1c0bd2ac-051e-4d4b-8b92-f1aa5825edaf observed 30.931 s and 30.814 s without convergence. Re-creation and the final restored-state stability check succeeded.

Final suite including initial absence on all blob surfaces: https://app.buildbuddy.io/invocation/c8b4d4eb-90c6-47f5-a8f5-ea2f2f9a2a8e. Blob sweep upper bounds 0.895–1.013 s; two-pass tree sweeps 2.620–5.076 s. All checks passed.

Final corrected deletion repeat: https://app.buildbuddy.io/invocation/b9727dc4-3e31-49ca-bf44-2b4417e65cb0. Proxy disagreement persisted 31.450 s and 30.795 s; every app missing-state projection and both endpoint re-creation controls passed.
