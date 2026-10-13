# BES target projection conformance

`bb remote test //tests/statemachine/target-projections:go_default_test --test_output=errors`

The test starts digest-pinned enterprise v2.314.0 app and cache proxy containers
using the harness. It publishes three CI test builds at distinct commits, with
two distinct labels: pass/flaky, fail/pass, then pass/flaky. Failed attempts
contain JUnit XML identifying the invocation, label and attempt. The reference
model retains prior builds and rereads them after each publication and complete
stream retry, checking that later builds neither overwrite nor mix earlier runs.
History uses bounded condition polling (30 seconds), not fixed sleeps.

## Released-source inventory

Inspected `~/buildbuddy-src` at v2.314.0:

| Mutating RPC / event | Read surfaces exercised | Assertions |
| --- | --- | --- |
| `google.devtools.build.v1.PublishBuildEvent/PublishBuildToolEventStream`: TargetConfigured / TargetCompleted | BuildBuddyService.GetInvocation and GetTarget, HTTP protolet equivalents, api.v1.GetTarget REST/gRPC | configured count, invocation target groups, label, aggregate status, per-build identity |
| same RPC: ActionExecuted | GetTarget HTTP/gRPC, api.v1.GetAction REST/gRPC | announced action count, label, configuration and invocation identity |
| same RPC: TestResult / TestSummary | GetTarget HTTP/gRPC, api.v1.GetTarget and GetAction REST/gRPC, `bb view` with target and test filters | summary status and attempts, isolated test-result details, action run/attempt identity, artifact URI equality, positive and negative CLI filters |
| same RPC: WorkspaceStatus / final event | GetTargetHistory HTTP/gRPC | exact label/invocation/status model after each build and retry, no duplicate history entries |
| same RPC: artifact references (CAS populated via ByteStream.Write) | app/proxy ByteStream.Read, UI `/file/download`, `bb download` on app/proxy | identical artifact bytes |

Schema and behavior references: `proto/build_event_stream.proto`,
`proto/buildbuddy_service.proto`, `proto/target.proto`, `proto/invocation.proto`,
`proto/api/v1/{service,target,action}.proto`,
`server/build_event_protocol/{event_index,target_tracker}/`,
`server/target/target.go`, `server/buildbuddy_server/buildbuddy_server.go`,
`server/api/common/common.go`, `enterprise/server/api/api_server.go`,
`enterprise/server/invocation_stat_service/invocation_stat_service.go`, and
`cli/{view,download,login}/`.

There are no target-specific create/update/delete RPCs. PublishLifecycleEvent
carries build lifecycle metadata, not target events. Invocation deletion belongs
to bes-lifecycle and is deliberately outside this item. The proxy exposes the
artifact cache surfaces, not the app target database projections.

`GetInvocation` intentionally returns top-level events and summarized target
groups, not individual test attempts. ActionCompleted and successful
TargetCompleted both map to API build actions; the expected count includes both.
Target-centric action expansion requires child IDs on TargetCompleted.
WorkspaceStatus triggers primary-DB target creation; finalization writes statuses.

## Unavailable surfaces and limitations

GetTargetStats, GetDailyTargetStats, GetTargetFlakeSamples and GetTargetTrends are
called over UI HTTP and asserted Unimplemented: these require OLAP (and trends
have a separate feature flag). Exhaustive stats/trends/flake conformance requires
coordinator-provided ClickHouse/OLAP harness support. No shared files were changed.
The scenario checks finalized builds, not intermediate persistence during a live
stream. It uses one configuration, run and shard with varying retry attempts;
multi-configuration transitions and sharded/multiple-run tests remain uncovered.
`bb download` checks referenced digest downloads, not `download artifacts` listing.

The CLI is release 5.0.492, fetched on the runner from the public GitHub release
asset `bazel-5.0.492-linux-x86_64`, pinned to SHA256
`e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.
Its subprocess environment excludes dispatch credentials; auth comes only from a
disposable Git repository configured with the fixture-created local API key.
All component RPC/HTTP destinations are fixture-local. Release download network
availability is a prerequisite.

App image: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
Proxy image: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`.
No confirmed product divergence has been observed.

## Verification

Final expanded targeted suite passed (42.2 seconds):
https://app.buildbuddy.io/invocation/5d75326c-e94d-4751-bdc9-a7ddffea0d0e

Earlier version including CLI, history and artifact surfaces also passed:
https://app.buildbuddy.io/invocation/51280a71-909f-466e-b0e4-b36566bec306
