# Directory / chunk structure convergence

Target: `//tests/statemachine/convergence-structures:go_default_test`.
Runs only fixture-local released app and proxy containers. SHA256, the default
instance, explicit 256 KiB average chunk size, 1 MiB read fallback threshold,
and enabled directory-size computation / proxy CDC. Every RPC has a deadline;
convergence is polled at 100 ms intervals for at most 30 seconds per step.

## Released-source inventory (v2.314.0)

| Operation | Mutation and read surfaces | Coverage |
| --- | --- | --- |
| CAS.BatchUpdateBlobs | Publishes file chunks and Directory protos; app/proxy FindMissingBlobs, BatchReadBlobs, ByteStream.Read, CAS.GetTree; UI gRPC/HTTP GetTree and GetTreeDirectorySizes; CLI directory extraction | Both endpoints; children before roots; shared-child DAGs; publication repeated through both upload APIs |
| ByteStream.Write | Same immutable CAS state as BatchUpdateBlobs | Both endpoints; new root updates file contents and removes a directory reference; old roots stay readable |
| CAS.SpliceBlob | Publishes validated ordered chunk manifest; app/proxy SplitBlob, FindMissingBlobs, BatchReadBlobs, ByteStream.Read, reconstructed chunk bytes; all tree/UI/CLI surfaces when referenced by files | Both endpoints; manifest-only full blobs; repeated registration; ordered mappings and exact bytes |
| CAS.SplitBlob | Read-only in this release: loads an existing manifest and checks children | After every state transition; both endpoints |
| CAS.GetChunkMapping | Explicitly Unimplemented | Valid complete read requests to both endpoints assert Unimplemented |
| CAS.RegisterChunkMapping | Explicitly Unimplemented | Valid complete streamed requests to both endpoints assert Unimplemented; model remains unchanged |

Source: `proto/remote_execution.proto` (CAS service, SpliceBlobRequest and
ChunkingFunction.UNKNOWN); `server/remote_cache/content_addressable_storage_server/`
(BatchUpdateBlobs, GetTree, spliceBlob, splitBlob, GetChunkMapping,
RegisterChunkMapping); `server/remote_cache/byte_stream_server/`;
`server/remote_cache/chunking/` (Manifest.Store validates presence, size and hash);
`enterprise/server/content_addressable_storage_server_proxy/` (forwarding);
`enterprise/server/byte_stream_server_proxy/` (CDC reads);
`proto/cache.proto`; `proto/buildbuddy_service.proto`;
`server/buildbuddy_server/` (UI adapter);
`server/remote_cache/directory_size/` (sizes include serialized Directory bytes,
count repeated edges); `server/http/protolet/` (framed HTTP streaming);
`cli/download/download.go`; `server/cache/dirtools/`.

Splice requests use UNKNOWN chunking function, whose protocol definition permits
opaque boundaries; all chunks are present and concatenate to the supplied hash.
SplitBlob does not generate chunks from flat blobs in this release. No unsupported
algorithm or malformed request is used. Each batch read is below the 4 MB limit.

CAS has no directory-update or directory-delete RPC. Structural updates/removals
publish new immutable roots; historical roots and all acknowledged children stay
in the reference model. Blob deletion belongs to the separate deletion item.

The model stores bytes, Directory messages, roots, and ordered chunk digests. Each
mutation checks all modeled CAS state on both endpoints; each published root is
compared against its reachable Directory set on app/proxy and UI gRPC/framed HTTP.
Recursive UI sizes and counts are compared to the model over repeated DAG edges,
and the unary gRPC size response is compared to HTTP. CLI extraction checks both
endpoints, file bytes, and absence of removed directory paths for each new root.
The CLI runs with only a disposable HOME and PATH, and receives only the fixture's
local key, never the dispatch environment.

The CLI accepts default-instance HASH/SIZE. An initial non-default instance
argument was rewritten by `cli/download` into a different resource namespace;
that trial was a client invocation mismatch, not evidence of app/proxy structure
propagation failure. The conformance target uses default-instance arguments.
CLI is the runner-installed `bb`; it is not a repository-built or pinned CLI.
Container versions/digests are pinned by the harness and logged by every run:

- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

## Validation

Green targeted run (27.4 s test):
https://app.buildbuddy.io/invocation/ea162010-7bad-4a6e-a8d5-32772ce84051
Runner invocation:
https://app.buildbuddy.io/invocation/128d279d-2239-4dfc-abb2-7d9b1e986d7f

Earlier development runs identified a too-large batch-read request and the CLI
resource-name mismatch described above; neither is classified as a product
finding. No app/proxy consistency defect was confirmed. No shared harness or
dependency change is required.
