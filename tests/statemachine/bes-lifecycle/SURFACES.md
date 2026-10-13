# Released v2.314.0 surface inventory

Inspected locally in `~/buildbuddy-src`; paths below are relative to that released
source, not dependencies used to build the tests. No BuildBuddy source is built.

| Surface | Implementation / frontend caller | Round-2 oracle |
|---|---|---|
| UI and gRPC `GetInvocation` | `proto/invocation.proto`, `server/buildbuddy_server`, `app/invocation/invocation.tsx` | ID, metadata, outcome, timing; independently decoded event payloads, with documented redaction/detail filtering |
| UI/gRPC `SearchInvocation` | `enterprise/server/invocation_search_service` | Exact one/zero records, metadata filters |
| UI/gRPC `GetInvocationStat`, `GetTrend` | `enterprise/server/invocation_stat_service` | Explicit count, outcome and duration; SQLite daily projections |
| REST/gRPC `api.v1.GetInvocation` | `enterprise/server/api/api_server.go` | ID and commit selectors; top-level model and complete metadata/workspace key sets |
| UI/gRPC `GetEventLogChunk` | `server/eventlog`, `app/invocation/invocation_logs_model.tsx` | Empty, live growing contents, completed contents and terminal empty-chunk pagination; deleted NotFound |
| gRPC `GetEventLog` | `proto/buildbuddy_service.proto`, `server/buildbuddy_server`, same frontend log model | Live first response after each progress write; completed concatenated chunks and EOF; deleted NotFound |
| `/file/download`, `/file/view`: raw_json/raw_proto | `serveArtifact`, `serveRawEventJSON/Proto`, `app/invocation/invocation_raw_logs_card.tsx` | Exact ordered decoded payloads; started options redacted and children omitted; progress console removed; isolated missing-resource contract repro |
| `/file/download`, `/file/view`: buildlog | `serveArtifact`, `app/invocation/invocation_build_logs_card.tsx` | Completed exact bytes; isolated deleted contract repro |
| UI/gRPC `GetTarget` | `server/target`, `server/build_event_protocol/event_index`, `app/invocation/invocation_artifacts_card.tsx`, `invocation_targets.tsx` | Populated target, exact label, artifact count, artifact-filter mode, HTTP/gRPC equality; deleted NotFound |
| REST/gRPC `api.v1.GetTarget` | `server/api/common`, `enterprise/server/api` | Populated label, status BUILT, rule type, language, tags; ID/label/tag selectors; deleted NotFound |
| REST/gRPC `api.v1.GetAction` | same | Two build action projections; target/config identity, stdout artifact URI/hash; deleted NotFound |
| `/file/download`, `/file/view`: bytestream_url | `app/service/rpc_service.ts`, `invocation_artifacts_card.tsx` | Named-set artifact uploaded through proxy, fetched through HTTP and both app/proxy ByteStream; CAS survives invocation deletion |
| `bb view` | `cli/view/view.go` | Known empty default-tail behavior during live progress; completed exact logs, post-delete error and no stale console; released CLI digest pinned |
| Proxy BES and invocation read/delete | `enterprise/server/cmd/cache_proxy/cache_proxy.go` | Lifecycle, streaming BES, GetInvocation and DeleteInvocation explicitly Unimplemented; no proxy invocation projection exists |
| `GetInvocationOwner` | `server/capabilities_filter` | Org caller PermissionDenied (requires server admin); no owner-state oracle |
| `GetInvocationFilterSuggestions` | `enterprise/server/invocation_search_service` | Explicit Unimplemented without OLAP |

## Additional state-dependent surfaces and limits

Source inventory also identifies OLAP `GetStatHeatmap`, `GetStatDrilldown`,
`GetTargetHistory`, `GetTargetStats`, `GetTargetTrends`, `GetTargetFlakeSamples`,
trend summaries/execution trends/quantiles. These are not validated with configured
ClickHouse. A shared fixture supporting a digest-pinned ClickHouse sidecar on the
owned Docker daemon is needed; the harness currently exposes no sidecar lifecycle
or daemon command API. No shared files were modified to add one.

Frontend action and timing cards additionally call `GetExecution`,
`GetExecutionDownloads`, `GetExecutionLog`, `GetTree`, bytestream profile/coverage/
execution-log fetches, and execution-profile downloads. These require real remote
execution or additional BES file types, not populated by this suite. The tested
artifact is a named-set output plus action stdout. Public invocation artifact
expansion and child-invocation metadata are not covered.

Default chunk/page selectors omit the first live log suffix, while explicit
`0000` selectors and streamed reads expose it. The separate `live_tail_repro`
asserts desired consistency; normal live coverage explicitly documents the
current empty default-tail behavior, rather than silently accepting missing
content.

Lifecycle notifications are accepted no-ops without configured BES forwarding
peers. They do not create local state, including repeated enqueue and started
notifications. Completion notifications are checked against all invocation model
readers plus raw/detail graph, logs and CLI. The proxy has no valid BES write
surface; valid app/proxy artifact byte agreement is checked instead.

Every progress update in the live sequence is followed by the invocation model
and all six log reader forms, with a second mandatory stable observation. Complete
publication, lifecycle notifications, replay, deletion and two-key re-creation
are checked separately. The initial started/metadata/workspace events remain a
batch: source deliberately buffers raw events and writes DB metadata once all
announced invocation metadata arrives. This suite does **not** claim every read
surface after every individual stream Send. Live raw/artifact downloads, configured
OLAP, large-page pagination and multiple identities (rather than two keys for the
same self-auth user) remain gaps against the requested universal contract.

Convergence logs are upper bounds including RPC observation time, not precise
backend commit latency. Polling is bounded at 20 seconds; streamed reads have
bounded contexts. Stability currently means two mandatory independent snapshots,
not a long-duration soak.
