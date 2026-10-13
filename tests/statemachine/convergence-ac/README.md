# Action-result replacement conformance

Target: `//tests/statemachine/convergence-ac:go_default_test`.

Released source inventory (`~/buildbuddy-src`, v2.314.0):

| Mutation | Read surfaces | Coverage |
| --- | --- | --- |
| ActionCache.UpdateActionResult | App and proxy GetActionResult; UI /file/view and /file/download actioncache reads; app UI HTTP/gRPC metadata | Absent → create → five replacements, alternating writers; repeated identical writes; every combination of inline stdout/stderr/output-file options; bounded convergence and subsequent regression checks |
| CAS.BatchUpdateBlobs | Both endpoints FindMissingBlobs, BatchReadBlobs, ByteStream.Read; UI /file/view and /file/download blob bytes; app UI HTTP/gRPC GetCacheMetadata; both Cache.GetMetadata; bb download | Valid Action, Command, input root and revision output blobs supplied; digest-backed output file plus alternating raw/digest-backed stdout/stderr |
| ByteStream.Write | Same blob read surfaces | Complete writes for revision stdout blobs |

References: `proto/remote_execution.proto` (ActionCache has only Get/Update,
no deletion); `server/remote_cache/action_cache_server/action_cache_server.go`
(AC persistence, output validation/inlining and server provenance);
`enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go`
(remote validation of cached response variants, no default TTL);
`server/cache_server/cache_server.go`, `proto/cache.proto`,
`proto/resource.proto`, `proto/cache_service.proto`, and
`server/buildbuddy_server/buildbuddy_server.go` (UI metadata delegates to
Cache.GetMetadata); `app/invocation/invocation_action_card.tsx`, `app/service/rpc_service.ts`,
`server/remote_cache/byte_stream_client/byte_stream_client.go` (UI HTTP actioncache/
bytestream downloads); `cli/download/download.go` (bb download reads CAS through
ByteStream, not AC). Scorecards describe invocation request history rather than
current AC state, and are outside this item's state oracle.

The reference model owns all user-supplied ActionResult fields and output bytes.
Only server-assigned execution provenance is excluded from equality. The release
retains digest-backed stdout/stderr even when their inline request flags are set;
the test checks that representation and reads their bytes on all blob surfaces.
UI AC metadata is app-side: proxy AC entries use salted request-variant keys and
CAS pointers internally, so comparing metadata at the original action key there
would be invalid. CAS metadata is checked on both endpoints after warming reads.
No delete operation is available in the scoped public service.

Images use the harness defaults, pinned enterprise-v2.314.0:

- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

All service traffic uses fixture-local containers and a disposable fixture API
key. CLI invocations use a temporary home and an explicit local target/key.
