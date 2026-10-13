# Invocation management conformance

`//tests/statemachine/invocation-management:go_default_test` tests released,
digest-pinned v2.314.0 app and cache proxy containers using a disposable local
organization. BES seeds one successful complete invocation; ingestion and target
projection semantics are outside this item's ownership.

| Mutation | Read surfaces / reference-model expectation |
| --- | --- |
| `buildbuddy.service.BuildBuddyService.UpdateInvocation` | Public read on → repeat → off → repeat → on. UI `GetInvocation` over gRPC and cookie-authenticated JSON protolet returns the current ACL; anonymous gRPC detail access follows public read. Ownership fields remain fixed in the detail ACL. Search, statistics and public API metadata remain visible to the authenticated organization. |
| `buildbuddy.service.BuildBuddyService.DeleteInvocation` | UI detail returns `NotFound` over gRPC and HTTP; search and statistics drop the invocation; `api.v1.ApiService.GetInvocation` over gRPC and REST returns an empty collection; build-log reads fail. Independent CAS bytes remain identical through app and proxy. Repeated deletion returns `NotFound` and leaves the model unchanged. |

Every state comparison uses fixture-local endpoints and bounded RPC contexts.
The cache proxy does not implement invocation management/read APIs; its applicable
shared state surface is ByteStream CAS. `bb view` runs with a fresh home and only
the disposable fixture key if the runner supplies a `bb` binary. Otherwise the
test logs the CLI availability limitation and checks the underlying
`GetEventLogChunk` gRPC and UI HTTP surfaces. No dispatch key is read or forwarded.

Release source inventory:

- `proto/buildbuddy_service.proto`: management and UI read RPC declarations.
- `proto/invocation.proto`, `proto/acl.proto`: ACL-only update schema, lookup,
  owner, search and invocation statistics schemas.
- `server/buildbuddy_server/buildbuddy_server.go`: mutation dispatch and owner.
- `server/backends/invocationdb/invocationdb.go`: ACL update (ownership remains
  fixed), synchronous deletion, permission checks and repeated-delete contract.
- `enterprise/server/api/api_server.go`: public invocation collection is queried
  from live SQL rows, including the empty-list contract after deletion.
- `server/eventlog/eventlog.go`: log reads require a surviving invocation row.
- `cli/view/view.go`: normal `bb view` reads run/build event log chunks.

Wire helpers encode only valid release-schema protobufs. The fixture saves exact
image IDs, configs and container logs in undeclared test outputs. No shared
harness or dependency changes are required.

`GetInvocationOwner` is declared server-admin-only by
`server/capabilities_filter/capabilities_filter.go`. The standard fixture provides
an organization admin, not a server admin; both gRPC and HTTP are consistently
`PermissionDenied` at every step. Owner-state verification needs a shared harness
option to configure `auth.admin_group_id` with the provisioned group's ID before
app startup (or a supported restart preserving app storage). No harness change
is made here.
