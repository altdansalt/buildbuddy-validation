# Cache hit accounting state machine

Released-source inventory (v2.314.0):

| Mutation | Read surfaces | Semantics |
| --- | --- | --- |
| `hit_tracker.HitTrackerService/Track` | `BuildBuddyService.GetCacheScoreCard` gRPC and UI JSON; `GetUsage` and `GetInvocation` gRPC and UI JSON | Append-only AC/CAS read/write records. Each accepted record counts once; replay counts again. Empty batches do nothing. |
| ByteStream.Write / Read | App and proxy ByteStream.Read; scorecard and usage above | Payload visibility and normal transfer accounting, with observer accounting isolated from workload metadata. |
| BES finalization (supporting fixture operation) | Scorecard | Detailed scorecards become readable after finalization, rather than during the running invocation. |

The reference model compares row multisets (including multiplicity), metadata,
cache/request type, digest size, compressor, and transferred size after each
finalized invocation. All eight AC/CAS × read/write × identity/ZSTD combinations
are covered. Repeated valid Track calls are intentionally additive, not assumed
idempotent: the API has no record or operation ID. There are no update/delete
RPCs in this service. Usage is checked against cumulative logical-byte and hit
counts. Locally served customer-proxy download bytes are checked in their separate projection
and excluded from the ordinary download total, as defined by UsageFields,
with bounded polling covering the fixed one-minute bucket, ten-second
settling time, and one-minute flush cadence. Scorecard/UI reads do not create
cache workload traffic.

Source references: `proto/hit_tracker.proto`, `proto/cache.proto`,
`proto/usage.proto`, `enterprise/server/hit_tracker_service/`,
`server/remote_cache/hit_tracker/`, `server/remote_cache/scorecard/`,
`enterprise/server/usage/`, `enterprise/server/usage_service/`,
`enterprise/server/execution_service/`, and `cli/` in the local released source.
No dedicated bb scorecard/usage command exists. GetExecutionDownloads reads
executor-produced auxiliary download metadata for a scheduled execution; Track
client hits do not populate that store. No executor is needed for this item.
The proxy does not serve the app's BuildBuddyService projections; the app is
the projection owner. Normal proxy cold reads forward to the app and count app-transferred bytes; warm reads report through the
fixture-local remote hit tracker. Bounded polling of live invocation counts
ensures the asynchronous warm-hit record arrives before BES finalization.

Fixtures log the exact image references and preserve configs/container metadata.
Default release: enterprise-v2.314.0, app digest
`sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`,
proxy digest
`sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`.

Run: `bb remote test //tests/statemachine/cache-hit-tracking/... --test_output=errors`.

Verified targeted suite:
https://app.buildbuddy.io/invocation/2722c19e-f444-488a-9eb1-69d49cdb8421
(1 test passed; app/proxy pinned images above). GetUsage gRPC uses a disposable
ORG_ADMIN API key; cache workload uses the ordinary fixture cache key. No
released-product inconsistency was confirmed. OLAP-only usage/export requires
ClickHouse and is outside the SQLite fixture; no shared harness change is needed.
