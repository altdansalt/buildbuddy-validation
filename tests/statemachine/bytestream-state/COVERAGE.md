# ByteStream state and surface inventory

| Mutating RPC | Checked surfaces | Sequences |
| --- | --- | --- |
| ByteStream.Write (identity/zstd) | Both replicas: identity/zstd Read and ranges, FindMissingBlobs, BatchReadBlobs digest/status/compressor/content, Cache.GetMetadata stored/logical sizes and access/modify times, required bb download identity/zstd. App: UI metadata gRPC, browser framed transport and cookie JSON with frontend compressor selection; API-v1 GetFile gRPC/HTTP; GetFileRange gRPC/HTTP first/interior/last/full ranges; cookie/request-context and API-key /file/download and /file/view | Absent → chunked complete → repeat → repeat alternate encoding using second same-org API key. All earlier model entries rechecked after every completion. Bounded presence convergence and three stable observations. |
| ByteStream.Write (Directory protobuf) | Above file surfaces plus both CAS.GetTree streams; UI GetTree gRPC and actual browser framed streaming transport; UI GetTreeDirectorySizes gRPC/JSON/browser (hash/total/child count); CLI Directory JSON and recursive output-directory contents | File publication checked before directory mutation; directory publication rechecks both bytes and tree. |
| ByteStream.Write (unfinished) | Both replicas CAS/ByteStream/metadata/CLI absence; app UI metadata/API-v1/HTTP absence; completed retry all file surfaces | Partial valid stream → close without finish → retry complete. |
| ByteStream.Write (ZIP archive) | All file surfaces; UI GetZipManifest gRPC/JSON/browser; /file/download and /file/view member extraction | Identity/zstd writes through each component; compare archive and extracted member after each write. |
| ByteStream.Write (compressed range probe) | Identity API range, compressed ByteStream range controls; compressed API-v1 range gRPC and HTTP | Persistent FailedPrecondition / HTTP 500 despite valid documented compressed URI. |
| API-v1 DeleteFile (explicit opt-in) | App CAS/ByteStream/metadata/CLI/UI/API-v1/HTTP absence; proxy all file surfaces retain reference bytes; create and re-create all file surfaces | Create via each component → warm → delete → repeat delete → re-create through warm proxy → delete again → re-create via second same-org app client. Isolated known-difference test. |

Source inspected at released v2.314.0: proto/{cache,cache_service,resource}.proto,
proto/api/v1/{file,service}.proto; server/remote_cache/byte_stream_server,
server/cache_server, server/buildbuddy_server; enterprise/server/api/api_server.go,
enterprise/server/cmd/cache_proxy; enterprise/server/backends/pebble_cache;
cli/download/download.go; app/service/rpc_service.ts,
app/invocation/{cache_requests_card,invocation_action_card,invocation_model}.tsx.

Frontend raw file/log/JSON views call fetchBytestreamFile through /file/download;
/file/view is also covered. Invocation raw_json/buildlog endpoints expose BES or
invocation data, not arbitrary ByteStream mutations, and are outside this model.
Action-cache and execution-download listing require different mutating services.
ByteStream blobs are immutable: updating bytes creates a new digest, while repeat
writes are idempotent. No ByteStream delete RPC exists. Proxy does not register
app UI or API-v1 services. QueryWriteStatus is checked for the released explicit
Unimplemented result, not treated as a resumable upload oracle.

Compressed ByteStream ranges are checked after decompression. API-v1 compressed
ranges bypass ByteStream transcoding; Pebble rejects passthrough compressed
ranges (FailedPrecondition) when representation is already compressed. This documented-URI mismatch is explicitly asserted in TestCompressedAPIRangeDivergence; identity
API-v1 ranges and complete compressed API-v1 downloads remain successful controls.

Metadata stored size and modification time must remain stable on repeated
metadata requests and match UI metadata within a replica. Access times are
positive; exact cross-replica timestamps are not meaningful. Cold proxy metadata
is deliberately checked before any read in its own isolated known-difference
test. Other metadata checks happen after materializing the replica.

Known-difference tests assert observed behavior and stay green. They are not
claims that local metadata or opt-in local deletion violates a product contract.
Tree and directory-size RPCs apply to Directory protobuf blobs; ordinary opaque blobs
have no valid tree interpretation. ZIP extraction and manifest metadata use a separate valid archive model in TestZipPublication.

Images: enterprise-v2.314.0 app
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`;
proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`.


## Convergence and known differences

Presence polling fails immediately on RPC errors, logs time since Write completion and the latency of the first
successful observation, bounds actual missing state at 10 seconds, then checks
three spaced stable observations. Full reference contents and every applicable
file surface are rechecked for every prior publication entry after each write.
Independent directory, ZIP, unfinished and deletion fixtures scope their models
to those sequences; directory blobs are checked as both opaque files and trees.

Cold metadata has no time-driven convergence in the ten-second observation
window; proxy Read materializes local metadata, followed by three stable checks.
DeleteFile is app-local and does not invalidate a warmed proxy; repeated deletion
leave app absent. A repeat proxy Write re-creates the app blob successfully (the opposite hypothesis was rejected). Source explains these
local-storage semantics; they are recorded as consistency divergences, not
confirmed contract violations. Compressed API ranges differ from the documented
API URI support and ByteStream range transcoding. See the finding files here.

No shared changes or new dependencies are required. CLI availability is mandatory.
The branch is pushed without creating a PR, per the task instruction.

## Verified runner observations

Full expanded suite: https://app.buildbuddy.io/invocation/456f3afc-6cb5-4eac-a1e2-307652ba4406
passed in 217.6 seconds. Prior expanded run also passed:
https://app.buildbuddy.io/invocation/4bec9624-d552-4dcf-b90a-43838447b7e1.

Every presence check in the full run succeeded on its first probe (0.36–3.14 ms
RPC latency). Logged times since Write completion span 0.50 ms–3.43 s because
full surface checks are serialized across model entries: these are observation
upper bounds, not measured replication delays. Three spaced presence checks
remained stable after each observation. Cold metadata persisted 10.06 s; proxy
read finished in 5.47 ms and metadata stayed stable over the next 309 ms.
Deleted bytes remained available at the warm proxy for each 2.03-second window;
re-upload through proxy restored all file surfaces within 0.89 s, including time
spent checking those surfaces. Compressed API range rejection remained stable
through 304 ms and is deterministic for the stored compressed representation.

The rejected hypotheses/expectation errors are preserved in invocation evidence:
GetFile absence uses the pooled client's explicit Unavailable rather than
NotFound; FindMissingBlobs queries authoritative upstream presence rather than
proxy byte availability; duplicate proxy Write does restore upstream bytes.
These expectations were corrected, never waived by ignored errors.

Absence HTTP assertions require exact released responses: API-v1 GetFile returns
404 with the pooled read failure; GetFileRange returns 500/NotFound via protolet.
Direct UI bytestream URLs without an invocation work while the blob exists; once
missing, the handler attempts invocation blobstore fallback and returns
400/Missing invocation_id param. This fixture has no BES invocation. The test
asserts that exact fallback response, alongside independent CAS/ByteStream,
UI metadata (including browser status trailers), API-v1 and CLI absence controls.
It does not accept arbitrary non-200 errors as proof of missing state.

Final full targeted verification with exact HTTP absence and ZIP JSON member
assertions passed in 217.6 seconds:
https://app.buildbuddy.io/invocation/c19f093e-dfd9-4258-9cd9-a0767725dbfa.
All three isolated differences reproduced again. Cold metadata: 10.048 s before
read-through, read completion 5.161 ms, stable checks through 308 ms. Deletion:
2.024–2.029 s observation windows, proxy re-create verified across every file
surface within 0.874–0.924 s. Compressed API ranges: persistent rejection over
304 ms, with both controls passing. Findings include both green invocation links.
