# Snapshot invalidation cross-surface conformance

Released v2.314.0 source inventory: `proto/{workflow,firecracker,cache,resource,buildbuddy_service}.proto`,
`enterprise/server/remote_execution/snaploader/snaploader.go`,
`enterprise/server/workflow/service/service.go`,
`enterprise/server/githubapp/githubapp.go`, `enterprise/server/auditlog/auditlog.go`,
`server/buildbuddy_server/buildbuddy_server.go`, `server/libmain/libmain.go`,
`app/service/rpc_service.ts`, `app/invocation/{invocation_action_card,workflow_rerun_button}.tsx`,
and `cli/remotebazel/remotebazel.go`.

| Mutation | Surfaces checked after every step | Result |
| --- | --- | --- |
| InvalidateSnapshot (gRPC and UI protolet) | App/proxy GetActionResult; app HTTP /file/download and /file/view targeting both caches; gRPC/UI GetCacheMetadata | Eight interleaved mutations across two instances, repeated operations and alternate ref. Fresh 10-character version on every mutation; all initialized and unrelated initialized entries agree. |
| InvalidateAllSnapshotsForRepo (gRPC and UI protolet) | Same surfaces | Three repeated valid unlinked URLs; verifies unchanged marker state after each individual call. Does **not** exercise linked-repo invalidation. |

Two locally provisioned keys in one org supply the app and proxy gRPC clients.
No worker credentials are used by the fixture. Both direct gRPC readers and all
HTTP readers are checked again after a 200 ms stability interval. Convergence
polling is bounded to 10 seconds at 100 ms intervals and logs elapsed time;
this measures reader agreement, not the instant an internal write propagated.
A random server-generated version cannot be predicted by an independent model:
the oracle asserts length, global uniqueness in the sequence, advancement, and
preservation of every unrelated known version, then compares exact metadata
bytes through every content reader. Metadata size is independently compared to
the serialized ActionResult size; mutable access timestamps are excluded.

The audit's passing download and UI metadata probes were incorporated with the
original expectations. No audit perturbations are retained.

## Remaining coverage gaps and surface boundaries

Snapshot invalidation has no create/list/delete API. Create-update-delete-recreate
of a real VM snapshot and restore rejection require Firecracker/KVM, unavailable
on this hosted runner. Direct writes/deletes of version markers would manipulate
implementation storage rather than exercise valid snapshot lifecycle operations.
The proxy is a cache data plane, not a BuildBuddyService mutation endpoint;
invalidation is routed to the app, while both caches are read.

`bb remote --run_from_snapshot` passes a snapshot key to execution; no dedicated
CLI snapshot reader exists. This execution path is **uncovered**, not counted as
passing. GetExecution, streaming execution responses and UI action details contain
historical VMMetadata; invalidation does not rewrite historical executions.
Invocation raw JSON and log artifacts likewise expose historical invocation data,
not the current version marker. `/file/view` and `/file/download` share the released
ServeHTTP/serveBytestream implementation and return binary ActionResult bytes for
ActionCache URLs; the UI fetchBytestreamFile uses the download path.

Linked-repository invalidation updates `GitRepositories.instance_name_suffix`.
GetLinkedGitHubRepos/GetWorkflows/GetWorkflowHistory do not expose that suffix.
LinkGitHubRepo requires a GitHub installation and authorized user repository
lookup (`findUserRepo`); the local self-auth fixture does not supply these. Actual
linked-repo execution/suffix transitions remain **uncovered**. Unlinked no-op
checks cannot detect an implementation that always no-ops.

GetAuditLogs gRPC/UI explicitly reports unavailable with the default fixture.
Audit emission remains **uncovered**: it needs audit_logs_enabled and a supported
OLAP database, not just the fixture's SQLite/Redis. No source-only observation is
reported as a runtime defect. Full requested coverage needs fixture-local GitHub
integration, OLAP, and a KVM-capable runner; no shared harness files were changed.

## Images and validation

Digest-pinned enterprise-v2.314.0 defaults:

- App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

Run only `bb remote test //tests/statemachine/snapshot-invalidation:go_default_test --test_output=all --nocache_test_results`.

Expanded cross-surface checks passed:
https://app.buildbuddy.io/invocation/bcc0accf-ed23-439d-90f4-3f98ab7402d0
No runtime divergence found. The full requested conformance scope remains incomplete
for the explicitly listed prerequisites above.

Final strengthened suite passed twice:
https://app.buildbuddy.io/invocation/d99075d7-96a9-48ea-9ffe-934e859f6ba2
https://app.buildbuddy.io/invocation/f73b12a4-8698-4866-a309-e476ceeeb3b9
The verbose run recorded 103.7 ms for step 0 and 206.7 ms for step 1
(one versus two serial instance polls). These include the poll scheduling delay;
they are agreement observation times, not evidence of a transient divergence.
Subsequent content/metadata reads and the 200 ms stability checks passed.
