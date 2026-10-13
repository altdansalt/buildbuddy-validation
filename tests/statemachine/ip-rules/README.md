# IP-rule state-machine consistency, round 2

Targets:

- `go_default_test`: full deterministic sequence, 5-second rule caches.
- `default_ttl_test`: create, enable, replace CIDR, delete with the released
  default 2-minute rule caches on both app and proxy.
- `artifact_download_enforcement_bug`: isolated app-only standards-expecting
  reproducer for raw JSON and build-log downloads; excluded from normal CI.
- `cache_download_enforcement_bug`: the same isolated repro with signed app
  identity, additionally checking CAS downloads; excluded from normal CI.

Run normal coverage with `bb remote test //tests/statemachine/ip-rules/...
--config=nobug --nocache_test_results --test_arg=-test.v`. Run repros explicitly
with `--test_tag_filters=bug`. All components and CLI calls run inside the remote
test; endpoints are fixture-local. No PR is opened for this round.

## Source inventory

Inspected released v2.314.0 `proto/{iprules,user,grp,auditlog,eventlog,invocation}.proto`,
`proto/api/v1/file.proto`, `server/buildbuddy_server/buildbuddy_server.go`,
`server/{http,rpc}/interceptors`, `server/libmain/libmain.go`,
`server/remote_cache/byte_stream_client`, `enterprise/server/ip_rules_service`,
`enterprise/server/ip_rules_enforcer`, `enterprise/server/auditlog`,
`enterprise/server/api/api_server.go`, and
`enterprise/server/cmd/cache_proxy/cache_proxy.go`.
Frontend callers: `enterprise/app/iprules/iprules.tsx`,
`enterprise/app/auditlogs/auditlogs.tsx`, `app/router/router.tsx`,
`enterprise/app/org/org_access_denied.tsx`, `app/service/rpc_service.ts`, and
`app/invocation/invocation_raw_logs_card.tsx`.
CLI callers: `cli/login/login.go`, `cli/ui/ui.go`.

There is no public IP-rule detail RPC, streaming rule-list RPC, rule download,
proxy-local rule list, or dedicated rule-management CLI command. `GetRule` is an
internal helper; `GetGroup` has no IP policy projection. The proxy registers
cache services, not BuildBuddyService: IP-rule writes/deletes through the proxy
are unsupported. Cache content is seeded through proxy and app and read through
both, with two org keys. IP-rule mutations alternate the two org keys on the app;
readers include both keys and the cookie-authenticated UI session. A separate
user-owned cache key exercises gRPC GetUser and the released CLI's login check.
Interactive `bb ui` calls the same GetUser RPC; its TUI is not launched.

## Transition-to-surface matrix

Every successful mutation is followed by the entire common observation below,
including unchanged projections. Initial state is also checked.

| Mutation | Exact state projections | Enforcement/other surfaces | Result |
| --- | --- | --- | --- |
| AddIPRule | Returned ID/CIDR/description; full gRPC lists using both org keys, UI JSON list, trusted list; gRPC/UI config; gRPC/UI GetUser; gRPC/UI audit history | Common enforcement probes below | See validation and finding |
| UpdateIPRule | Complete lists, config, GetUser, audit resource's previous description and updated request payload | Common probes | See validation and finding |
| DeleteIPRule | Exact remaining lists including duplicates; config, GetUser, audit deleted ID/description/request | Common probes | See validation and finding |
| SetIPRulesConfig | Config booleans; GetUser group enforcement and selected-group ID/access; stored lists; trusted list empty when disabled; audit config events including repeated calls | Common probes | See validation and finding |

Common probes use four IPv4 documentation addresses: admin anchor, normal /32,
inside replacement /24, and outside replacement /24. The CIDR reference model
computes authorization; there is no independently maintained allow boolean.

- App/proxy CAS.FindMissingBlobs and ByteStream.Read, with two org API keys.
  Allowed reads must return exact seeded bytes; denied calls must return
  PermissionDenied. Proxy and app seeding exercise both directions.
- gRPC/UI GetUser: group flag and selected-group decision. gRPC GetUser is
  intercepted with PermissionDenied for forbidden IPs; the UI endpoint remains
  readable and reports DENIED_BY_IP_RULES. Allowed gRPC calls verify the full
  relevant user projection, using a valid user-owned key.
- HTTP GetIPRules and GetIPRulesConfig: success versus IP-rules HTTP 403.
- gRPC/UI GetInvocation metadata, gRPC/UI GetEventLogChunk, and gRPC streaming
  GetEventLog: readable exact seeded invocation/log versus authorization denial.
- HTTP `/api/v1/GetFile`: exact bytes versus HTTP 403.
- UI `/file/download` CAS, raw JSON, and build-log routes: exact baseline content
  after every step, explicitly recording the known enforcement divergence.
- Released `bb` 5.0.492 `login --check`: success versus permission error, through a
  fixture-local GetUser-only gRPC forwarder that supplies the controlled IP.
  CLI has no IP flag; the forwarder neither changes replies nor grants access.

Audit assertions compare the complete ordered IP-mutation history, not just an
entry count: operation, generated/existing resource ID, resource description,
request CIDR/description/config payload, actor key label, and client IP. Repeated
operations remain separate events. The app fills AddIPRule's generated ID into
its logged request; request_context is deliberately cleared by the logger.
A directory-local, pinned ClickHouse sidecar enables both audit readers. No
shared harness/dependency changes are needed.

## Sequences and convergence

Full sequence: initial state; create admin; create normal while disabled;
enable; repeat enable; replace /32 with /24; repeat update; restore /32; create
duplicate; delete one duplicate; delete final duplicate; re-create; delete
re-created entry; disable; repeat disable; delete anchor while disabled.
Each operation succeeds and immediately advances the reference model.
Missing-ID deletes are excluded because they return NotFound rather than a
successful mutation. The local audit's retained HTTP probes were brought over
from `audit/ip-rules-local` (7ff5ed8), without its experimental perturbations.

The full sequence polls all projections every 250ms with a 25s bound, then
requires six uninterrupted seconds of agreement, exceeding its 5s rule TTL.
Any mismatch resets the stable interval. Default-rule-TTL coverage uses a 150s
bound and 1s poll interval, retaining the six-second stability requirement.
CLI checks run after sustained convergence at every step. Test logs report
observed convergence time and stability duration, not a fixed assumed delay.

Fixtures enable IP rules, trusted X-Forwarded-For, fixture-only signing secrets,
remote rules on the proxy, signed app identity for internal cache reads, user
keys and audit logging. Auth/JWT caches remain explicitly 1s even in the
**default-rule-TTL** target; this is not an all-default-configuration latency
claim. Normal fixture defaults have IP rules and audit logging disabled.

## Pinned artifacts

- App enterprise-v2.314.0:
  `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- Proxy enterprise-v2.314.0:
  `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
- ClickHouse 25.3:
  `mirror.gcr.io/clickhouse/clickhouse-server@sha256:b627d7a9bc0e0c1bac26cdbe9d2fc6316faa29c5d8a174f28f5abd57d0fa6ba2`
- bb 5.0.492 Linux x86_64 binary SHA256:
  `e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.
  Downloaded inside the test from the pinned GitHub release. CLI subprocesses
  receive a minimal environment and only a disposable repo-local user key.

## Findings and validation

See [app-ip-rules-download-enforcement.md](app-ip-rules-download-enforcement.md).
Validation invocation links and measured convergence are recorded there and in
this section after the final runs.
