# BES lifecycle conformance

Target: `//tests/statemachine/bes-lifecycle:conformance`.
Run with `bb remote test //tests/statemachine/bes-lifecycle:conformance --test_output=errors`.

The test runs released enterprise-v2.314.0 containers, never source-built servers:

- App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
- CLI: released 5.0.492 linux-x86_64, SHA256 `e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.

All service requests use fixture-local addresses and disposable credentials.
The CLI subprocess gets a fresh HOME and an explicit environment with the local
key only. Its binary is downloaded from the pinned public GitHub release and
verified before execution. No dispatch key is read by the test.

## Source inventory at v2.314.0

| Mutation | Supported read surfaces | Coverage |
| --- | --- | --- |
| `google.devtools.build.v1.PublishBuildEvent.PublishLifecycleEvent` | No locally persisted projection: `server/build_event_protocol/build_event_server/build_event_server.go` forwards to configured peers only | Enqueued (including repeat), attempt started, attempt finished, build finished; no invocation from enqueue, completed invocation unaffected by completion notifications |
| `PublishBuildToolEventStream` | `BuildBuddyService.GetInvocation`, `SearchInvocation`, `GetInvocationStat`, `GetTrend`; UI JSON protolet; `api.v1.ApiService.GetInvocation` REST and gRPC; log endpoints and `bb view` | Active stream, ordered ACKs, component-stream completion and EOF, success/failure completion, full valid replay; identity, repo/commit/branch metadata and selectors, role, status, creation/update timestamps, duration, outcome, stats/trend counts; exact console output |
| `BuildBuddyService.DeleteInvocation` (cleanup transition) | Invocation detail, search, stats, trends, public API | Removed from every listed reader over HTTP and gRPC |

The reference model advances from partial to complete with a specified outcome
and duration. Started announces metadata children, and metadata precedes the
workspace-status flush barrier, matching the event graph contract. Bounded
polling waits for state, identity and a converged timestamp snapshot; there are no fixed startup sleeps or
canceled/malformed event streams. Completion/replay is synchronized by all
ordered ACKs and terminal EOF. Reflection reads the running release's message
descriptors for direct gRPC calls; small Bazel event payloads use the released
`proto/build_event_stream.proto` wire field numbers.

Reader implementations inspected: `server/buildbuddy_server`,
`enterprise/server/invocation_search_service`,
`enterprise/server/invocation_stat_service`, `enterprise/server/api`, and
`cli/view`. The cache proxy registration in
`enterprise/server/cmd/cache_proxy/cache_proxy.go` exposes cache APIs only;
it has no invocation projection or BES forwarding service. The test verifies
its lifecycle RPC returns Unimplemented, rather than inventing an agreement
requirement for a state surface it does not expose.

## SQLite boundaries and scope

`GetInvocationOwner` is server-admin-only in `server/capabilities_filter`; the
fixture org user/key receives PermissionDenied over both HTTP and gRPC, which
is checked explicitly. It cannot serve as an org-authorized state projection.

`GetInvocationFilterSuggestions` requires OLAP and is explicitly checked for
Unimplemented over both HTTP and gRPC. Daily invocation `GetTrend` and `GetInvocationStat` use SQLite.
Invocation summaries, execution trends, quantiles, finer time buckets, target
trends, heatmaps and drilldown require OLAP/configuration and are not exercised.
Their absence is not a consistency finding.

Round 2 adds populated target/action/named-set artifact projections, full raw
JSON/protobuf and detail event contents, streaming logs and terminal pagination,
post-delete logs/CLI/commit selectors, live progress updates with two stable
observations, and same-ID deletion/re-creation with two API keys in the same org.
See [SURFACES.md](SURFACES.md) for implementation/caller inventory and explicit
remaining limits. This remains short of EVERY surface after EVERY stream send;
in particular configured OLAP needs shared sidecar support.

Known divergences: [deleted download error mapping](finding-deleted-download.md)
and [default live-log tail omission](finding-live-default-tail.md).
Normal `conformance` is untagged with respect to `bug`. The isolated
`deleted_download_repro` and `live_tail_repro` targets are tagged `bug` and intentionally fails while
this error mapping remains. Run it explicitly with `--test_tag_filters=bug
--nocache_test_results`. No PR is opened for this round; only the assigned branch
is pushed.
