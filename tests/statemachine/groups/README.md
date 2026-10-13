# Organization configuration and status

Source inventory: BuildBuddy v2.314.0, `proto/grp.proto`, `proto/user.proto`,
`proto/bazel_config.proto`, `server/buildbuddy_server/buildbuddy_server.go`,
`server/capabilities_filter/capabilities_filter.go`,
`enterprise/server/backends/{userdb,authdb}`, `server/util/groupstatus`,
`enterprise/server/remoteauth`, and `cli/cli_command`.
Initial bot-suggestion and developer-org-creation values follow the release's
`tables.Group` GORM defaults (true even for proto3 zero values on create); updates
explicitly test disabling and re-enabling both. This is not a cross-surface
disagreement: both GetUser transports expose the same stored defaults.

The embedded schemas preserve release field numbers and types; comments are omitted.
They are parsed in the test with the scaffold's existing dependencies.

| Mutation | Read surfaces and reference-model assertions |
| --- | --- |
| CreateGroup | Real gRPC, two independent organizations; GetGroup by URL and admin ID over gRPC and UI JSON protolet; GetUser membership projections over both transports; GetBazelConfig over both transports; normal app and proxy cache reads |
| UpdateGroup | Real gRPC plus cookie-authenticated protolet for domain ownership (requires a user identity); name, URL identifier, owned domain, sharing, group-owned executors, user-owned keys, bot suggestions, developer org creation, code search, clean-workflow restriction, and all non-default suggestion preferences; repeated identical updates; old URL ceases resolving; every other organization retains its model |
| SetGroupStatus | Real gRPC using a locally provisioned server-admin impersonation key; free, enterprise, enterprise trial, usage based, open source, blocked, and free again; repeated identical status sets; membership status and normal API authorization on app and proxy |

GetGroup intentionally exposes public identity rather than policy/status.
GetBazelConfig configuration options must stay stable across these policy transitions;
credential visibility changes when personal keys are disabled. Successful reads must
agree across transports, allowing hidden admin-only organization credentials to
be absent from the user-key response. Disabled personal keys and blocked API requests must be
rejected, while web membership/configuration reads and admin recovery remain available.
The test uses bounded polling (20 seconds, 100ms ticker) after every modeled operation.
Auth cache TTLs are explicitly one second on both fixtures, within that bound;
production defaults are five minutes on the app and one minute on the proxy.

The app uses its built-in single-user identity and default on-prem organization as
server-admin group. It is set to enterprise status and allows developer org creation
to satisfy the release's API creation prerequisites. All credentials are created through fixture-local authenticated
APIs. No database writes or synthetic signed credentials are used.
There is no group-delete RPC and no bb organization-list command. The cache proxy
has no group-management RPC or organization UI read endpoint; it is checked through
normal authenticated cache requests. Workflow execution, suggestions, code search,
and developer-role enforcement are not exercised here: their configuration
projections are covered, while personal-key and blocked-status policies are tested
through effective authorization. Those feature-specific effects need separate
fixtures and identities; no shared harness changes are required for this target.

Pinned enterprise-v2.314.0 images:

* App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
* Proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

Run: `bb remote test //tests/statemachine/groups:go_default_test --test_output=errors`.
