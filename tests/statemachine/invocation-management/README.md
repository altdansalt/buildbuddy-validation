# Invocation management conformance, round 2

Released, digest-pinned v2.314.0 app and cache proxy; all component work runs
inside fixture-local containers on `bb remote`. The audit's GetTarget probe is
integrated into every reference-model check; no perturbations were imported.

| Mutation | Surfaces checked after every normal-suite step | Result |
| --- | --- | --- |
| UpdateInvocation | UI detail, owner, search ACL, statistics build count, target, target history and trends over gRPC and JSON protolet; anonymous detail/target and streaming logs follow ACL; unary logs gRPC/HTTP; public GetInvocation/GetLog/GetTarget/GetAction gRPC/REST; raw JSON/protobuf/buildlog download/view; normal/error/target/test-filter `bb view`; associated test.log bytes through app/proxy and HTTP download/view | Consistent. Public API invocation schema has no ACL field; its projection remains readable by the org. |
| DeleteInvocation | All the above reads, second org API key, repeat-delete and re-create with the same ID | Consistent except permanent owner/download status discrepancies documented in adjacent findings. Cache bytes intentionally survive invocation deletion. |
| DeleteInvocation with OLAP | GetTrend gRPC plus HTTP trends, statistics, target history and filter suggestions snapshots before/after delete | OLAP build count remains 1 after five seconds; isolated strict repro. |

The sequence creates an invocation, enables/repeats/disables/repeats/enables
public read using two API keys in one org, deletes via the second key, repeats
deletion via the first, then re-creates the same ID through BES. Every state is
checked twice with a stability interval. A ten-second bounded poll checks detail
ACL/existence before the full surface comparison, failing on unexpected RPC
errors. Detail converged on the first observed read (about 1–2 ms); the full
comparison and stability checks took about 1.7 seconds in the instrumented run.
These are observation bounds, not measurements of an asynchronous delay.

`bb` is required: no unavailable-CLI skip. CLI credentials are the disposable
fixture key and a fresh home, never worker dispatch credentials. Negative checks
use specific gRPC codes or specific absence messages, including the CLI's
human-readable filtered-target errors.

BES events include CI metadata, configured/expanded test targets in tracker-valid
order, result/summary events, and a CAS-backed test.log uploaded via the proxy.
The UI and public API expose its reference. App/proxy ByteStream and HTTP artifact
reads check exact bytes before and after each management operation. The proxy
registers only cache/ByteStream/CAS/AC/capabilities/OCI/fetch services: it has no
BES or invocation mutation/read service. Invocation updates/deletes via proxy
are therefore not valid supported operations. Cache retention is the applicable
cross-component invariant; it is not invocation-row replication.

Owner coverage configures `auth.admin_group_id` on the disposable group by
restarting the app before ingestion, preserving fixture storage and refreshing
published ports and cookie login. An ORG_ADMIN key observes the actual owner.
Target history enables `app.enable_target_tracking` on the app. The OLAP repro
starts a separate pinned ClickHouse 25.3 sidecar on the same fixture network,
configures `olap_database.data_source`, and enables OLAP target read/write flags.
No shared harness/dependency modifications were made.

Run passing coverage:

```
bb remote test //tests/statemachine/invocation-management/... --config=nobug --test_output=errors
```

Run strict repros (expected failures on this release):

```
bb remote test //tests/statemachine/invocation-management:divergences_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

`cached_api_test` tests deletion with non-default `api.enable_cache=true`; that
candidate divergence was rejected because GetTarget correctly returned NotFound.

Source inventory, inspected locally at v2.314.0:

- `proto/buildbuddy_service.proto`, `invocation.proto`, `target.proto`,
  `stats.proto`, `eventlog.proto`, `cache.proto`, `api/v1/{invocation,target,action,log}.proto`.
- `server/buildbuddy_server/buildbuddy_server.go`: management, owner,
  artifact/view/download routing and cache metadata dispatch.
- `server/backends/invocationdb/invocationdb.go`: synchronous SQL mutation,
  ownership permissions, collection ACL projection and missing-row mapping.
- `server/target/target.go`, `server/build_event_protocol/target_tracker/target_tracker.go`:
  primary/OLAP target history and ingestion prerequisites.
- `enterprise/server/{api,invocation_search_service,invocation_stat_service}`:
  public projections, optional Redis API cache, trends and OLAP suggestions.
- `server/eventlog/eventlog.go`, `server/remote_cache/scorecard/scorecard.go`:
  log and cache-scorecard authorization/storage.
- `enterprise/server/cmd/cache_proxy/cache_proxy.go`: supported proxy services.
- `app/service/rpc_service.ts`, `app/invocation/{invocation_logs_model,invocation_not_found,invocation_action_card,cache_requests_card}.tsx`,
  `app/target/flaky_target_chip.tsx`: real UI downloads, streaming, owner,
  execution, metadata and target-stat surfaces.
- `cli/view/view.go`, `server/api/common/common.go`: filtered/error/test output
  and public action file projections.

Remaining limitations: this is broader coverage, not an exhaustive every-surface
claim. Execution records/downloads, populated cache scorecards, zip manifests,
OLAP heatmaps/drilldowns, flake samples/target statistics and run-log state need
richer RBE/run fixtures. A GetCacheMetadata probe hit an app-internal
localhost:1985 Unavailable error before any mutation; it was excluded pending
fixture/routing investigation, not classified as a product divergence. Its
invocation was https://app.buildbuddy.io/invocation/73d71f65-2578-49b1-aaa1-d316e4cc491c.
OLAP checks currently exercise deletion, not every ACL toggle or re-creation.

Validation: passing normal/cached suite at
https://app.buildbuddy.io/invocation/32cefb6b-0364-4020-beaa-f30b389b05ba.
The three adjacent finding files contain both independent reproduction links.
No PR is opened, per the round-2 task instruction.
