# Snapshot invalidation control-plane coverage

Source inventory: released v2.314.0 checkout, `proto/workflow.proto`,
`proto/firecracker.proto`, `proto/auditlog.proto`,
`server/buildbuddy_server/buildbuddy_server.go`,
`enterprise/server/remote_execution/snaploader/snaploader.go`,
`enterprise/server/workflow/service/service.go`,
`enterprise/server/auditlog/auditlog.go`, `enterprise/app/workflows/workflows.tsx`,
and `cli/remotebazel/remotebazel.go`.

| Mutation | Read surfaces and runnable result |
| --- | --- |
| InvalidateSnapshot | App and proxy ActionCache.GetActionResult version metadata: absent → initialized → advanced on repeats; unrelated instance unchanged; alternate refs share the invalidation marker. Mutations exercised through gRPC and cookie-authenticated UI HTTP protolet. |
| InvalidateAllSnapshotsForRepo | gRPC and UI HTTP accept valid unlinked repository URLs, including repeats. Existing snapshot markers for unrelated instances remain unchanged. This is only unlinked-repository no-op coverage. |
| Both | GetAuditLogs through gRPC and UI HTTP explicitly reports unavailable in the baseline fixture. Audit events are not validated. |

The test keeps a reference map of current versions by instance. It reads the
app's generated version after each mutation, verifies that it advanced, then
polls both read surfaces against the model, including unaffected instances.
The version key calculation follows the released snaploader implementation;
protobuf field numbers follow the released protos. No fixture state is injected
through SQL, private storage, or direct cache writes.

There is no snapshot Get/List/Delete RPC or `bb` snapshot reader. `bb remote
--run_from_snapshot` drives execution rather than reading invalidation state.
GetWorkflows/GetWorkflowHistory/GetLinkedGitHubRepos do not expose the current
repository instance suffix. Repository invalidation changes
GitRepositories.instance_name_suffix for an existing linked repository; the
fixture cannot provision that relationship without external GitHub installation
and user authorization. Arbitrary accepted URLs must not be described as linked
repository invalidation validation.

Execution details (GetExecution and its UI HTTP endpoint, and stored
ExecuteResponse auxiliary VMMetadata) can contain historical snapshot identities.
They require a real Firecracker execution to populate; invalidation does not
rewrite historical execution metadata. No such execution is attempted without
KVM. Version-marker agreement does not validate snapshot creation, reuse, or
restore rejection.

Audit logging requires `app.audit_logs_enabled` and an OLAP database. Enabling
only the flag fails app startup; this harness provisions SQLite and Redis, not
ClickHouse. Source declares actions INVALIDATE_VM_SNAPSHOT (14) and
INVALIDATE_ALL_WORKFLOW_VM_SNAPSHOTS (11). The latter request type is absent from
the audit Entry.APIRequest union in this release; this is a source observation,
not a confirmed runtime finding. Testing audit emission requires a supported
fixture-local OLAP service. No harness or dependency changes are made here.

Images are the harness digest-pinned defaults:

- App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

Both are enterprise-v2.314.0. Fixture artifacts preserve actual image inspect
metadata and container logs.

Run: `bb remote test //tests/statemachine/snapshot-invalidation:go_default_test --test_output=errors`.

Validation: [passing targeted test](https://app.buildbuddy.io/invocation/5298ec0d-41b5-406a-be31-b1d7b8b0c26e).
The initial [run](https://app.buildbuddy.io/invocation/f2d20344-dd4b-468e-98c2-14f4562ce875)
passed the snapshot transitions but stopped at repository invalidation because
the baseline key lacked ORG_ADMIN. The final test provisions its own local
admin key and passes the complete sequence. No product inconsistency confirmed.
