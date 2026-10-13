# User creation and organization membership

Target: `//tests/statemachine/users-membership:go_default_test`.
Run with `bb remote test //tests/statemachine/users-membership:go_default_test --test_output=errors`.

The test starts the released enterprise app using the harness and a test-owned
OIDC issuer on the remote runner. Two separate browser cookie jars represent
admin and member identities. The issuer signs real RS256 ID tokens and implements
OIDC discovery, JWKS, authorization codes, and token exchange. All app traffic
addresses the fixture container; all issuer traffic addresses this test's server.
There are no external identity providers, dispatch credentials, database edits,
or source-built components. The issuer selects the host interface using a UDP
connect to a documentation address without sending any packet; no component
request uses that address. The test client explicitly rejects other destinations.

The app uses the harness default pinned release:
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
(`enterprise-v2.314.0`). The harness preserves actual image IDs, configs and logs.

## Source inventory (v2.314.0)

| Mutation | Implementation / semantics | Read surfaces exercised |
| --- | --- | --- |
| `CreateUser` | `server/buildbuddy_server/buildbuddy_server.go:482`; OIDC profile and new generated user ID. `enterprise/server/backends/userdb/userdb.go:1010` rejects repeated creation with FailedPrecondition. | HTTP `GetUser` immediately; HTTP and gRPC `GetUser` identity and group projections after organization setup; organization roster via HTTP/gRPC `GetGroupUsers`. |
| `JoinGroup` | `buildbuddy_server.go:868`; `userdb.go:570` creates REQUESTED membership, or MEMBER when the email matches an owned domain. Existing requests/members return AlreadyExists. | HTTP/gRPC `GetUser`, `GetGroupUsers` filtered by MEMBER, REQUESTED, and their union, and public `GetGroup`; authenticated HTTP organization reads. |
| `UpdateGroupUsers` | `buildbuddy_server.go:560`; `userdb.go:819` supports ADD, REMOVE, and role-only updates. ADD accepts pending requests or directly adds identities, repeated ADD returns AlreadyExists, REMOVE is idempotent. | Same surfaces; all four roles additionally check user capabilities, selected group, UI allowedRpc, and actual GetGroupUsers authorization as the member. |

Schemas: `proto/user.proto`, `proto/grp.proto`, `proto/user_id.proto`,
`proto/context.proto`, `proto/capability.proto`, and
`proto/buildbuddy_service.proto`. `server/capabilities_filter/capabilities_filter.go`
restricts organization roster reads and updates to admins.

Public gRPC authentication in `enterprise/server/oidc/oidc.go:499` accepts API
keys and trusted internal claims, but does not establish the provider user token
required by `FillUser` (`oidc.go:697`). Therefore CreateUser is exercised through
the supported cookie-authenticated UI protolet HTTP route, not with fabricated
internal credentials. JoinGroup and every membership update use real public
gRPC with locally provisioned keys. User-owned home-organization keys provide
identity for GetUser and JoinGroup; a separate target-organization admin key
provides authority for roster reads and updates. GetUser omits the requested
group on gRPC because API keys are organization-scoped; its returned user_group
still exposes the user's full database membership and role capabilities.
Cookie-authenticated HTTP GetUser selects the target group explicitly.

`GetGroup` is public metadata, not a membership reader. The test looks it up by
URL identifier (ID lookup requires server-admin privilege) and checks stable ID
and name from both identities on both transports after every operation.
The cache proxy registers cache services, not BuildBuddyService
(`enterprise/server/cmd/cache_proxy/cache_proxy.go:382`); it has no user or
membership projection. Searching `cli/` finds no dedicated membership reader.
No cache proxy or CLI membership surface is invented for this item.

## Reference model and limits

Each step updates an independent `(membership status, role)` model. Reads must
converge within 25 seconds, using a ticker to drive bounded polling instead of
fixed sleeps. Both identities retain a separate home organization so removal
cannot accidentally destroy the ability to authenticate. The target admin
remains a member throughout. Checks cover request/repeat/cancel, repeat removal,
pending role change, approval, repeat ADD and JoinGroup, batched updates,
developer/writer/reader/admin transitions,
repeat role update, member removal, direct ADD, owned-domain direct JoinGroup,
removal and rejoining, and fresh-session reads after final removal. Duplicate
CreateUser is rejected and its existing identity must survive.

Only direct identity membership is in scope. User-list membership belongs to the
separate user-list API area. There is no user deletion RPC in this inventory.
CreateGroup, UpdateGroup (owned-domain policy), and API-key creation are fixture
setup. The small protobuf wire adapter uses the released schema field numbers,
normal valid messages and the standard proto codec content type; it does not
exercise malformed wire requests. No shared harness or dependency changes are
needed, and no product inconsistency is assumed from setup errors.

## Round 2 additions

After every step (not snapshots) the model is compared with: HTTP+gRPC `GetUser`,
`GetGroupUsers` (3 status filters, no unexpected rows), `GetGroup`, selected
group/allowedRpc, target-org `GetUserApiKeys`/`GetUserApiKey` (capability mask
incl. the sticky role-downgrade rule in `userdb.updateUserRole`),
`AuthService.Authenticate` JWT claims, CAS/AC/ByteStream/Capabilities through
**both** app and cache proxy (writes via one, reads via the other), public API
`GetFile`/`GetFileRange`, `/file/download`, HTTP+gRPC `GetCacheMetadata`, and
`bb ui` (pinned CLI, PTY). Polls are bounded (25 s) and the converged state must
hold for 1 s. The suite runs with `auth.enable_anonymous_usage=false` and
`--auth.remote.jwt_cache_ttl=2s`; the default-TTL proxy test asserts eventual
convergence. Known divergences (`bug`-tagged, in `findings/`):
`:revocation_bug` (proxy serves removed key ~59 s) and `:download_bug`
(anonymous usage on).
