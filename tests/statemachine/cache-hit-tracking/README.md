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

## Round 2 surfaces and sequence coverage

The audit transfer probe was restored from `audit-cache-hit-tracking-local`;
none of `audit-perturbations.patch` was imported. `Upload` is correctly treated
as CAS.BatchUpdateBlobs; `Write` exercises actual ByteStream.Write.

| Mutating RPC | Surfaces asserted | Sequence / result |
| --- | --- | --- |
| HitTracker.Track | Live GetInvocation gRPC/UI counters after every request; finalized GetCacheScoreCard gRPC/UI row multisets, status, supplied duration, filters, stability; paginated SearchInvocation gRPC/UI; GetTrend and GetInvocationStat gRPC/UI; GetUsage gRPC/UI including daily/monthly sums and download/upload breakdowns | Duplicate append then empty no-op. Eight accounting combinations; isolated usage append/repeat/no-op waits after each request. |
| ByteStream.Write | App/proxy ByteStream.Read, CAS.BatchReadBlobs, CAS.FindMissingBlobs; GetCacheMetadata gRPC/UI; `/file/download`, `/file/view`; `bb download` against app/proxy; public GetFile streaming gRPC/REST and GetFileRange gRPC/REST | Identity/ZSTD create, second same-org key rewrites through proxy, CAS batch rewrite through app; every step checked twice, separated by 250ms. |
| CAS.BatchUpdateBlobs | Same data surfaces as ByteStream.Write; live invocation accounting and finalized accounting projections in the original workload | Real batch rewrite of each streamed blob; immutable digest/payload reference model. |
| BES finalization | Detailed scorecard, history pagination, trends and invocation count | Each completed invocation checked; detailed scorecards are persisted at finalization. |

`TestScorecardPagination` crosses the released 100-row boundary using 101
independent digests, checks ascending size order and continuation tokens over
both transports, then repeats the entire traversal. Synthetic successful
transfers assert zero miss counters, status OK, a supplied 1ms duration and
logical-byte throughput. Malformed numeric values and unknown enum values fail;
omitted proto default zeros remain valid. Protobuf digests use `proto.Equal`.

The CLI is released 5.0.492, fetched inside the test from its immutable release
URL and verified with SHA256
`e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.
Its environment contains only a temporary HOME and system PATH. The CLI receives
only disposable fixture-local keys and fixture endpoint addresses.

Further source inventory: `server/buildbuddy_server/buildbuddy_server.go`
ServeHTTP handles both file routes; `app/service/rpc_service.ts` and
`app/invocation/cache_requests_card.tsx` call metadata, scorecard, and file
endpoints; `enterprise/app/history` and `enterprise/app/trends` use search and
stats; `enterprise/server/invocation_stat_service` defines the compact history
and aggregate projections. `proto/cache.proto` defines timing, filters and
pagination; `cli/download/download.go` defines the CLI blob download surface.
Public read API definitions were checked in `proto/api/v1/{file,service}.proto`
and `enterprise/server/api/api_server.go`. Trends include customer-proxy bytes while GetUsage exposes those bytes in a
separate bucket. These are different documented projections, not divergence.

Track does not modify CAS or AC contents. It cannot create a downloadable
artifact, modify raw BES JSON/logs, or produce execution streaming metadata.
There are no update/delete RPCs in HitTrackerService and no public delete in
CAS/ByteStream; destructive invocation management and the separately gated public DeleteFile
mutation are not exercised by these tests. QueryWriteStatus is deliberately Unimplemented
in the released server. OLAP-only heatmaps/drilldowns and detailed OLAP usage dimensions need
ClickHouse, which this SQLite fixture does not provide.

Convergence times are logged as observed upper bounds (RPC/poll/sample overhead
included), not claimed as an exact server latency. Live accounting uses bounded
30s polling followed by two stable samples. Usage has fixed minute buckets and
uses a 150s bound followed by two stable samples. Transfer visibility is asserted
immediately after acknowledged writes, then again after 250ms. Finalized
scorecard state is checked again after 250ms.

Remaining limits: usage-per-step coverage uses eight-way Track batches;
the eight-way accounting matrix still checks cumulative usage at the end.
Scorecard and trends cannot be checked during each in-progress BES step because
their detailed/persisted state is finalized later. Timing for actual network
transfers is nondeterministic; exact duration/throughput assertions use supplied
Track durations. Cross-surface transfer observers deliberately omit invocation
metadata so their reads do not pollute the accounting workload's reference model.
No shared harness changes are requested.

Round 2 also compares history cacheStats to the per-invocation reference model,
checks scorecard search filters on both transports, repeats persisted history and
trend projections after 250ms, and validates daily usage sums, period selection,
and download/upload breakdowns across gRPC and HTTP.

Delete semantics inventory: public DeleteFile requires the non-default
`--enable_cache_delete_api=true` gate. Released proxy ByteStream/CAS reads are
local-first immutable replicas; app deletion is not a purge of warmed proxy CAS.
This is a documented separate-cache retention contract, not evidence that Track
accounting diverges. The proxy does not expose a DeleteFile RPC.
