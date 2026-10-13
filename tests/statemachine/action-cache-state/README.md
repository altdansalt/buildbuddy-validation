# Action-result creation and replacement

Target: `//tests/statemachine/action-cache-state:go_default_test`.
Run with `bb remote test //tests/statemachine/action-cache-state/... --test_output=errors`.

Released fixtures: enterprise-v2.314.0, app
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`, proxy
`gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`.
Harness overrides remain supported and logs record actual images. All calls target
fixture-local containers. The CLI inherits only PATH and a temporary HOME; no
worker API key enters a child command. `bb` is supplied by the hosted runner.

## Mutation and read-surface inventory

Inspection of the v2.314.0 source in `~/buildbuddy-src`:

| Mutation | Read surfaces | Coverage |
| --- | --- | --- |
| REv2 ActionCache.UpdateActionResult | GetActionResult; Cache.GetMetadata; BuildBuddyService.GetCacheMetadata HTTP JSON and gRPC | Absent → create → repeat → replace → repeat → replace; latest exit code, output digest/path/executable flag, streams and serialized AC size |
| CAS.BatchUpdateBlobs (prerequisites) | BatchReadBlobs, FindMissingBlobs, ByteStream.Read, Cache.GetMetadata, UI metadata HTTP/gRPC | Valid Action, Command, input root/file and stdout/stderr; result-reference blobs checked after every accepted write |
| ByteStream.Write (output prerequisite) | Same CAS surfaces; bb download | Complete valid output writes, repeated uploads, old outputs retained after replacement; CLI bytes equal model |

`proto/remote_execution.proto`, `server/remote_cache/action_cache_server/action_cache_server.go`
(GetActionResult, UpdateActionResult, fetchActionResult, maybeInlineOutputFiles),
`enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go`,
`proto/cache_service.proto`, `proto/cache.proto`, `proto/resource.proto`,
`server/cache_server/cache_server.go`,
`server/buildbuddy_server/buildbuddy_server.go` (GetCacheMetadata),
`enterprise/server/cmd/cache_proxy/cache_proxy.go`, and
`cli/download/download.go` establish this inventory. The metadata RPCs are
read-only. ActionCache has no delete RPC; eviction is not a valid lifecycle
mutation for this item. GetTree is a Directory traversal surface, not an
ActionResult surface. No execution, malformed references or missing-blob
behavior is involved.

Each service runs its own instance and reference model. Every write is checked
synchronously at that endpoint, including all eight combinations of file,
stdout and stderr inline options. Small output files must inline when selected;
stdout/stderr may remain digest-backed (the released app does not implement
these inline hints), but both projections must resolve to the same bytes.
The final revision stores raw stdout/stderr to cover that representation too.
Immutable output history is re-read after replacement. Metadata size is checked
against the accepted result, including server-added execution metadata.

The app UI APIs expose app storage. Proxy writes reach the app synchronously,
so their app metadata is checked too. Proxy Cache.GetMetadata is checked for
locally materialized CAS blobs. Proxy AC storage instead contains pointers under
hashes of GetActionResult requests (including inline options and a salt), not
under the original Action digest; this internal representation is not a public
AC metadata oracle. Proxy convergence after direct app writes belongs to the
separate convergence item and is intentionally not exercised here.

The metadata helper uses a scoped protobuf wire codec matching the released
schemas, avoiding a shared dependency or harness change. It checks digest sizes,
not last-access timestamps that legitimately differ between surfaces. The app
fixture raises `cache.pebble.min_bytes_auto_zstd_compression` to 1 MiB so small
AC entries have deterministic stored sizes equal to serialized accepted results;
`digest_size_bytes` instead remains the Action size. All RPCs and CLI commands have a bounded context; no sleeps.

## Validation

Final expanded target passed uncached (24 seconds):
https://app.buildbuddy.io/invocation/900e021d-d99d-405a-be3b-7b4610342d00
(outer runner: https://app.buildbuddy.io/invocation/81313f6b-86bb-4ef1-b007-7d523427e5f5).
Earlier lifecycle target also passed:
https://app.buildbuddy.io/invocation/f47068f1-c7ff-44f4-bd95-2cec0e37d14d.
No confirmed product inconsistency was observed. During development, assertions
were corrected for protobuf internal caches and the distinct digest/stored-size
semantics, including default on-disk zstd compression; these were test errors,
not findings. No shared harness/dependency changes are required.
