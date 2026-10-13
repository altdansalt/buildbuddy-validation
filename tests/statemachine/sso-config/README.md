# Organization SSO state conformance

Released app: `gcr.io/flame-public/buildbuddy-app-enterprise:enterprise-v2.314.0`,
pinned to `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
The harness records the actual image for overrides.

| Mutation | Read surfaces | Expected transition |
| --- | --- | --- |
| `SetSSOConfig` | gRPC (two keys) and UI HTTP `GetSSOConfig`; authenticated/anonymous gRPC and HTTP `GetGroup.ssoEnabled`; server-admin gRPC/HTTP lookup by ID; anonymous HTTP subdomain lookup; SAML metadata and login destination; untouched org config | DB projections agree immediately; cached SAML provider converges within 135s; immediate-clear bug isolated |

The reference state is the URL from the last acknowledged mutation. Every step
checks both authenticated HTTP reads, authenticated gRPC reads, anonymous gRPC
and HTTP GetGroup, and the second admin key's GetSSOConfig and GetGroup. The
untouched organization must retain its empty SSO config. The default suite also
checks server-admin GetGroup(group_id) through gRPC/HTTP and anonymous HTTP
GetGroup with the slug derived from the Host subdomain. Writers alternate between
two independent organization-admin keys. The sequence is create → update → update
→ clear → recreate → repeat → clear → repeat. JSON scalar omission is accepted
only for protobuf defaults; present scalars must have the correct type.

With SAML enabled, each step checks SP metadata and the login redirect's actual
IdP destination. Reads are polled for at most 135 seconds, and then checked three
more times over three seconds for stability. Observed convergence is logged per
step. Metadata uses an actual trusted TLS server in the app network namespace.
The fixtures distinguish the IdP destinations for the two documents.

`immediate_consistency_repro` is a separate `bug` target: set, prime the provider,
clear, check the database surfaces, then require immediate metadata withdrawal.
`replacement_consistency_repro` sets, primes, replaces, and requires the new
IdP login destination immediately. The default coverage target accepts bounded
convergence; the bug targets preserve the stronger immediate consistency
requirement. All requests address the disposable app and fixture-local metadata
server, with valid metadata and no malformed assertions.

## Released-source inventory and remaining limits

- `proto/{buildbuddy_service,grp}.proto`,
  `server/buildbuddy_server/buildbuddy_server.go`: SetSSOConfig is the sole SSO
  mutation. GetSSOConfig returns the URL; GetGroup derives ssoEnabled. GetUser's
  group message has no organization SSO field. Config.ssoEnabled is global support.
- `enterprise/app/settings/sso_config.tsx`: settings calls Get/SetSSOConfig.
  `enterprise/app/login/login.tsx`: login calls anonymous GetGroup and then login
  with the organization slug. These HTTP reads and login are exercised.
- `enterprise/server/saml/saml.go`: metadata, login, ACS and logout share the
  two-minute provider cache. Metadata XML does not expose the IdP URL, so login
  destination is the replacement oracle. ACS signed-assertion acceptance and
  logout session behavior still need a signed local IdP fixture; they are not
  claimed covered by testing metadata/login. The shared module currently has no
  SAML/XML-signature library; a coordinator-approved dependency or signed-IdP
  harness fixture is needed to verify valid ACS/session behavior reliably.
- Server-admin GetGroup(group_id) and subdomain-derived HTTP GetGroup are checked
  after every transition. The suite enables subdomain matching and configures its
  generated group as the server-admin group before the state-machine starts.
  Restarting the disposable app during setup requires refreshing published ports
  and reconnecting the web session; no restart occurs during tested transitions.
- `enterprise/server/auditlog/auditlog.go`: UPDATE_SSO_CONFIG records require
  audit logging plus OLAP storage. Audit-log reads remain untested; a disposable
  ClickHouse fixture/dependency is needed. Source-only suspicion (unverified):
  auditlog.proto Entry.APIRequest has no SetSSOConfigRequest member, while
  wrapRequestProto rejects payloads absent from that union. GetAuditLogs must be
  exercised against configured OLAP before this can become a confirmed finding.
  The permitted mirror returned 404 for clickhouse/clickhouse-server:24.8 and
  :24.8.4.13 and the released-source test version :25.3; no Docker Hub pull was attempted. Parent-group cascades remain open.
- No SSO reader exists in `cli/`; cache proxy registers cache services rather
  than organization RPCs. Proxy writes/reads and app/proxy convergence are
  inapplicable to this state. No download, raw JSON/log or streaming SSO reader
  was found in proto, server, enterprise or frontend source.

## Running

```
bb remote --runner_exec_properties=init-dockerd=true test //tests/statemachine/sso-config:go_default_test --test_output=all
bb remote --runner_exec_properties=init-dockerd=true test //tests/statemachine/sso-config:immediate_consistency_repro //tests/statemachine/sso-config:replacement_consistency_repro --test_tag_filters=bug --nocache_test_results --test_output=all
```

The bug targets intentionally fail on the pinned release. No PR is opened.

## Round-2 observations

[Expanded admin/subdomain suite](https://app.buildbuddy.io/invocation/ac2b45fc-b92e-43e1-a6c6-35a82e727791)
passed with replacement convergence of 117.012s and 117.017s, and clear
convergence of 117.010s and 113.012s. The latter clear follows a repeated write
which does not renew the cached provider. Each converged state remained stable
across three additional reads over three seconds. New providers, repeated sets,
and repeated clears matched at the first one-second polling observation.
Database read surfaces agreed immediately before every convergence poll.

Isolated clear reproductions:
[run 1](https://app.buildbuddy.io/invocation/acb84ea9-3557-4e2a-b25e-345e2a3103b1),
[run 2](https://app.buildbuddy.io/invocation/f15e37c3-12df-47dd-a08f-da965f68bf3f).
Isolated replacement reproductions:
[run 1](https://app.buildbuddy.io/invocation/cfbc0601-16a6-470a-a2c9-417f2bbbb5ed),
[run 2](https://app.buildbuddy.io/invocation/31271f0f-93bf-4295-8ea9-a0990a9b68cf).
[Both final bug targets](https://app.buildbuddy.io/invocation/31d4a805-bc92-41e6-98c9-dda06664d1c4)
also fail with explicit metadata/login HTTP statuses and the stale destination.
The associated finding is [FINDING-saml-provider-cache.md](FINDING-saml-provider-cache.md).

[Final coverage run, including login withdrawal after clear](https://app.buildbuddy.io/invocation/545fe696-aa16-4782-8e01-43f7f1dfa443)
passed. Replacement observations: 117.013s and 117.013s; clear observations:
117.011s and 113.011s. All subsequent stability assertions passed.
