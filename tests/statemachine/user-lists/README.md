# User-list state machine

Target: `//tests/statemachine/user-lists:go_default_test`.

Released app: `enterprise-v2.314.0`, pinned by the harness to
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.

| Mutation (gRPC) | Read surfaces after every operation |
| --- | --- |
| CreateUserList | GetUserLists and GetUserList, gRPC and cookie-authenticated JSON protolet |
| UpdateUserList | Same four surfaces; stable ID, renamed name, unchanged membership |
| UpdateUserListMembership | Same four surfaces; exact member ID/email sets |
| DeleteUserList | Both listings omit ID; both details report missing; surviving list unchanged |

An independent model records list identity, name, and member sets. The fixture's
real user is resolved through GetGroupUsers on both transports. Two lists share
that identity, enabling checks that removing membership or deleting one list
preserves the other. Repeated rename succeeds; repeated add returns AlreadyExists;
repeated removal and deletion return NotFound without changing observable state.
Recreating a deleted name and repeating creation allocate new identities.
Renaming a populated list preserves its members. Every transition checks all
surfaces immediately: the app's SQL-backed mutations are synchronous, so no
sleep or eventual-consistency polling is needed.

Source enumeration at released tag v2.314.0:
- `proto/buildbuddy_service.proto`, `proto/user_list.proto`, `proto/grp.proto`,
  `proto/user_id.proto`: four mutations, detail/list readers, identity lookup.
- `server/buildbuddy_server/buildbuddy_server.go:879-1003`: RPC implementations
  and optional audit emission.
- `enterprise/server/backends/userdb/userdb.go:1359-1572`: transactional SQL state,
  member sets, and repeat-operation status contracts.
- `enterprise/app/org/org_user_lists.tsx` and `org_user_list.tsx`: UI uses these
  same readers through `/rpc/BuildBuddyService/<method>`.
- `enterprise/server/cmd/cache_proxy/cache_proxy.go:382-388`: proxy does not
  register BuildBuddyService and exposes no user-list state.
- `cli/`: no user-list management reader.
- `enterprise/server/auditlog/auditlog.go`: audit logging requires a configured
  OLAP database; the baseline harness does not provide one. Audit readback is
  outside this fixture's coverage.

Only the app container is needed. gRPC uses a disposable ORG_ADMIN key
created by the fixture web session; the default cache key is insufficient. Self-auth supplies one distinct user, so
coverage includes empty and singleton sets and multi-list independence; it does
not establish behavior for several distinct users. No shared harness changes.
