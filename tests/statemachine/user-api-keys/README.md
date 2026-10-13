# User API key state machine

Released image defaults: enterprise-v2.314.0, app
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
and proxy
`gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`.
The harness logs actual images and preserves container/config artifacts.

## Source inventory

Inspection of the released source in `~/buildbuddy-src`:

| Mutation | Definition / implementation | Checked surfaces |
| --- | --- | --- |
| BuildBuddyService.CreateUserApiKey | proto/buildbuddy_service.proto; proto/api_key.proto; server/buildbuddy_server/buildbuddy_server.go | Creation response; GetUserApiKey and GetUserApiKeys via gRPC and cookie-authenticated UI protolet HTTP; GetApiKeys exclusion via both transports; authenticated GetUser; app/proxy cache reads and effective writes |
| api.v1.ApiService.CreateUserApiKey | proto/api/v1/{service,api_key}.proto; enterprise/server/api/api_server.go | gRPC creation and public /api/v1/CreateUserApiKey JSON creation; all of the same read surfaces |
| BuildBuddyService.UpdateUserApiKey | proto/api_key.proto; server/buildbuddy_server/buildbuddy_server.go | All four key readers, org-list exclusion, authenticated identity and app/proxy capability enforcement |
| BuildBuddyService.DeleteUserApiKey | Same files | Both single-key readers report absence, both lists omit the key, app/proxy authenticated CAS reads eventually reject the key |

`enterprise/server/backends/authdb/authdb.go` implements shared storage, membership
masking, capability normalization and key authorization. `CACHE_WRITE` implies
`CAS_WRITE`; effective reads remove redundant CAS_WRITE. Public creation chooses
role-derived capabilities, whereas UI creation takes explicit capabilities.
`server/http/protolet/protolet.go` defines the JSON HTTP serialization, including
camelCase response names. No public list/update/delete user-key RPC and no
corresponding `cli/` reader exist. The proxy does not expose user-key management;
its state surface is authentication and cache authorization.

## Model and sequence

The test maintains an ID-indexed model of labels, effective capabilities, secret
values, and presence. Each creation transport creates two same-label keys;
creation is intentionally non-idempotent. Each key receives CAS-only, full-write,
and read-only updates, each repeated twice, then deletion. Every mutation is
followed by comparisons against all four key readers. Lists must omit secrets;
single-key reads must preserve secrets; visibility remains group-admin-only.
Owner identity is checked using GetUser authenticated by each user key. User keys
must not appear in organization-key lists.

Valid CAS and action-cache write probes check effective capabilities using actual
stored state, since read-only writes intentionally return success without storing.
Each probe uses a fresh digest and compares administrative reads on both app and
proxy. Auth and capability caches use one-second fixture-local TTLs, with bounded
20-second polling at 100ms intervals. RPCs and HTTP requests have deadlines.
These TTLs shorten observation latency without disabling authentication caches.

Run only:

```
bb remote test //tests/statemachine/user-api-keys/... --test_output=errors
```

The fixture uses the built-in single local self-auth identity. Multiple-owner
isolation requires a mock OIDC identity provider and is outside this fixture's
coverage. Deprecated `visible_to_developers=true` is explicitly invalid for user
keys on update, so it is not submitted. User-key readers omit `user_owned` and
expiry fields by construction; ownership is checked through identity and list
membership instead. Optional expiry and generated TLS certificates are not
exercised. Repeating delete after successful deletion targets a nonexistent ID
and is not treated as a valid idempotent operation.

## Validation

Targeted remote run passed:
https://app.buildbuddy.io/invocation/a551da12-2f93-4f3f-81dc-5ff2e62991ad

Final test passed with `--nocache_test_results`, checking effective capabilities
following each repeated update individually:
https://app.buildbuddy.io/invocation/b5bf6231-548d-4956-a409-3a08da16ad7e

No confirmed cross-surface divergence was observed. Earlier development failures
were corrected request/serialization/model assumptions, not product findings.
