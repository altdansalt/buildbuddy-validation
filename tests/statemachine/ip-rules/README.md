# IP-rule state-machine consistency

Target: `//tests/statemachine/ip-rules:go_default_test`.

Released app and cache proxy: enterprise-v2.314.0, harness-pinned manifests:

- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`

## Source inventory

Inspected released source in `proto/iprules.proto`,
`proto/buildbuddy_service.proto`, `server/buildbuddy_server/buildbuddy_server.go`,
`enterprise/server/ip_rules_service/ip_rules_service.go`,
`enterprise/server/ip_rules_enforcer/ip_rules_enforcer.go`,
`enterprise/server/clientidentity/clientidentity.go`,
`enterprise/app/iprules/iprules.tsx`, and `cli/`.

| Mutating gRPC RPC | Read surfaces checked after each transition |
| --- | --- |
| AddIPRule | returned entry detail; BuildBuddyService.GetIPRules gRPC and UI JSON HTTP; trusted IPRulesService.GetIPRules; app/proxy authorized CAS requests; config readers |
| UpdateIPRule | complete entry fields and IDs through both list projections and trusted reader; app/proxy enforcement; config readers |
| DeleteIPRule | absence through all list projections; app/proxy enforcement; config readers |
| SetIPRulesConfig | GetIPRulesConfig gRPC and UI HTTP; GetUser UI group enforcement flag; stored lists remain intact; trusted reader returns empty while disabled; app/proxy enforcement |

There is no public entry-detail read RPC (GetRule is an internal helper), no
proxy-local rule list API, and no bb rule-management command. The proxy's
remote rule provider is tested through normal requests and its trusted backend
read contract, rather than treating its private memory as a public surface.

The model covers initial empty state, create, enable twice, change CIDR and
revoke a normal client, repeat update, restore access, repeat create (distinct
IDs), delete overlapping rules individually, disable twice, and delete the
admin anchor only while disabled. Repeated delete of a missing ID is excluded:
the public service calls GetRule first and returns NotFound, so it is not a
valid successful mutation. Every observation polls actual RPC state with a
25-second deadline, without fixed convergence sleeps.

The fixture is explicitly a trusted-forwarder deployment: both services trust
X-Forwarded-For, and the test supplies two controlled documentation-range client
addresses. An admin /32 remains allowed throughout enforcement. A separate
local ORG_ADMIN key performs management; ordinary fixture cache keys perform
policy probes. Client identity signing uses only a disposable fixture secret.
Rule and auth caches have one-second TTLs so propagation fits the bounded test.
This verifies eventual consistency, not default-TTL propagation latency.

Audit records are unavailable in the existing SQLite/Redis fixture. The test
asserts GetAuditLogs reports Unimplemented. Source registers an audit logger
only with `app.audit_logs_enabled` and an OLAP database. Adding a fixture-local
OLAP service requires shared harness work and is left to the coordinator.

## Validation

Targeted remote test passed: [invocation](https://app.buildbuddy.io/invocation/c0bb006e-23ad-430e-a9a0-4fcf2548c562).
The initial setup-only failure (organization URL missing) was corrected before
validation; it is not a product finding. No observed cross-surface divergence.
