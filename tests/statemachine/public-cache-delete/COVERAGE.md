# Public cache deletion state machine

Release: enterprise app v2.314.0, default pinned image
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
Actual image/configuration is also recorded by the fixture in test artifacts.

Source inventory (released checkout):

* `proto/api/v1/service.proto`, `file.proto` and
  `enterprise/server/api/api_server.go`: DeleteFile (gRPC and JSON REST) deletes
  CAS or AC, accepts namespaced resource names, treats missing entries as success,
  and requires `enable_cache_delete_api`. GetFile streams CAS or serialized AC using the URI host;
  GetFileRange reads local CAS only.
* `proto/remote_execution.proto`, CAS/ByteStream/ActionCache servers:
  BatchUpdateBlobs and ByteStream.Write create immutable CAS; UpdateActionResult
  creates/replaces AC; FindMissingBlobs, BatchReadBlobs, ByteStream.Read and
  GetActionResult expose the corresponding state. Valid Action and Command
  protos are uploaded before publishing their results. Explicit worker metadata
  makes the reference model independent of server-added defaults.
* `proto/cache.proto`, `resource.proto`, `buildbuddy_service.proto` and
  `server/buildbuddy_server/buildbuddy_server.go`: GetCacheMetadata exposes both
  CAS and AC through UI gRPC and authenticated JSON protolet HTTP. `/file/download` also exposes CAS and
  serialized AC. A fixture-local BES invocation supplies valid download context.
* `cli/download/download.go`: `bb download` reads CAS through ByteStream; it has
  no AC download mode. Tested with explicit fixture-local target/key for the
  default instance. Nested instance resources are checked through other readers;
  CLI's digest argument parser prepends `/blobs/` to namespaced arguments.

Every model check compares both targeted and unrelated CAS/AC keys. The model
tracks CAS presence and complete AC results, including output file digests.
Sequences run with empty and nested instance names: upload, create AC, replace
and repeat AC, gRPC delete, REST repeated delete, recreate, delete both AC
references, CAS delete/repeat/recreate through both transports. AC deletion must
leave output CAS intact. Deleted CAS is recreated with ByteStream.Write.

| Mutation | Read surfaces |
| --- | --- |
| BatchUpdateBlobs / ByteStream.Write / DeleteFile CAS (gRPC, REST) | FindMissingBlobs, BatchReadBlobs, ByteStream.Read, GetFile gRPC/HTTP, GetFileRange gRPC/HTTP, UI file/download, UI GetCacheMetadata gRPC/HTTP, bb download (default instance) |
| UpdateActionResult / DeleteFile AC (gRPC, REST) | GetActionResult, GetFile gRPC/HTTP, UI file/download, UI GetCacheMetadata gRPC/HTTP, all CAS readers for immutable outputs and unrelated blob |

The proxy does not expose this public mutation API; warmed proxy invalidation is
owned by convergence-deletes and deliberately excluded. The test never contacts
shared deployments and does not inject failures. RPC and HTTP operations share a
bounded context. App-local writes/deletes are synchronous, so their readers are
checked immediately. The app self-dialed metadata client may initially be in
startup reconnect backoff; only Unavailable is retried with bounded ticker
polling, never an inconsistent presence/absence result. GetFile intentionally
wraps missing-resource errors as Unavailable; the test checks empty payload and
corroborates absence using direct cache reads and HTTP 404 responses.

The small protobuf wire codec uses only the documented released field numbers
and avoids requiring shared harness or dependency changes for upstream bindings.

Validation: targeted `bb remote test //tests/statemachine/public-cache-delete/...
--test_output=errors` passed (24.7s test duration):
https://app.buildbuddy.io/invocation/efea1418-12d0-4259-803a-6d90db686049
Runner: https://app.buildbuddy.io/invocation/96a1bc5e-02d9-4429-9e61-dc17d80b461d

No confirmed state divergence was observed. Earlier development runs corrected
startup self-client readiness, explicit AC metadata, and missing-resource error
expectations using the released implementation; these are not product findings.
