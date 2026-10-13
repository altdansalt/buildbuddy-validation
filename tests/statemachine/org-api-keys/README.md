# Organization API-key state machine

Target: `//tests/statemachine/org-api-keys:go_default_test`.
Released enterprise v2.314.0, harness digest-pinned app
`sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
and proxy
`sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`.

## Surface inventory and coverage

| Mutation | Read surfaces and expectations |
| --- | --- |
| CreateApiKey | gRPC and cookie HTTP GetApiKey, GetApiKeys, GetBazelConfig; app Authenticate JWT capabilities/group; authenticated app/proxy CAS reads |
| UpdateApiKey | Same surfaces after CACHE_WRITE → CAS_WRITE → CACHE_WRITE, label replacement/clearing and developer visibility changes; repeat identical update |
| DeleteApiKey | Detail NotFound over gRPC / HTTP error; disappearance from both lists and Bazel credential sets; app Authenticate rejection and app/proxy CAS Unauthenticated |
| CreateImpersonationApiKey | Source inventory only: requires server-admin group; one-hour expiry; hidden from ordinary group lists. Not reachable with default fixture organization authorization. |

Two full cycles test repeated creation without treating Create as idempotent.
Delete is not idempotent: the service reads the existing key before deleting it;
repeating a delete on an absent ID is not included in the valid-operation sequence.
Each operation updates a small reference model. Read surfaces compare their
supported projections; list and Bazel credentials must never expose secrets.
Bazel credentials omit capabilities in this release (`toProtoAPIKeys`), so that
surface checks identity, label, visibility and presence. Detail/list compare
capabilities; Authenticate verifies claims and both components verify acceptance.

Supported configuration flags shorten app API-key cache and proxy JWT cache TTLs
to one second (defaults five minutes and one minute). Authentication checks retry
on a ticker until observations converge, with a 20-second deadline; no blind
sleep or clock manipulation. Requests only address test-local fixtures.

## Source inventory (released tag)

- `proto/buildbuddy_service.proto`, `proto/api_key.proto`, `proto/context.proto`,
  `proto/bazel_config.proto`, `proto/auth.proto`, `proto/capability.proto`.
- `server/buildbuddy_server/buildbuddy_server.go`: all four mutation handlers,
  GetApiKey/GetApiKeys, GetBazelConfig, toProtoAPIKeys.
- `enterprise/server/backends/authdb/authdb.go`: valid capabilities, visibility,
  expiry, admin-only impersonation, key cache TTL and list filtering.
- `enterprise/server/remoteauth/remoteauth.go`: proxy JWT cache TTL.
- `server/util/claims/claims.go`: JWT fields and server-admin membership.
- `enterprise/server/auditlog/auditlog.go`: mutation audit events require
  `app.audit_logs_enabled` plus configured OLAP storage.
- `cli/`: API-key consumers exist, but no dedicated key-management read command.

## Remaining prerequisites

No audit checks: fixture does not provision OLAP audit storage. No impersonation
creation/expiry check: server-admin group ID must be configured before app startup,
while fixture organization ID is generated after startup. Coordinator harness
support for an admin bootstrap/restart would enable valid impersonation requests;
its fixed one-hour expiry additionally needs a long-timeout test. No harness or
shared dependency edits were made. This coverage does not claim exhaustive
impersonation or audit conformance, certificate generation, or default-cache TTL
validation.

## Validation

`bb remote test //tests/statemachine/org-api-keys:go_default_test --test_output=errors`
passed: https://app.buildbuddy.io/invocation/bfa8a055-683b-4692-88d3-cc6e15d01e30
(remote dispatch: https://app.buildbuddy.io/invocation/9a281a62-7765-43c8-8d2a-d99c5a720415).
Initial projection-assumption failure:
https://app.buildbuddy.io/invocation/f1414d0d-7bf5-4002-81c9-4114cf260bb0;
corrected against the released implementation, not recorded as a product defect.

## Round 2 additions

The lifecycle now executes the actual runner `bb login --check` against the local
app, with a temporary Git repository and disposable local key. The CLI environment
is limited to PATH, HOME and TMPDIR, without reading the dispatch key. It also retains the audit's
GetUser acceptance rule and AC persistence oracle: writes through app and proxy
are read through both components; CAS_WRITE must not persist AC entries. CAS
uploads through either component are read through both. These run after each
create/update, and CLI/auth/CAS rejection is checked after each delete.

Creation expectations come from the request, with independent distinct-ID checks.
Inventory checks require exact baseline-plus-live-key counts, no duplicate IDs,
and preservation of unrelated key projections. Creation timestamps must be
present and remain unchanged across updates. Capabilities are compared as sets
because the release's bitmask conversion does not guarantee array order. Modern
visibility output is checked against the deprecated requests in the green suite;
modern input semantics have a separate failing contract repro. Cross-group
GetApiKey requires PermissionDenied; disabled certificate generation requires
FailedPrecondition. HTTP deletion must specifically contain gRPC NotFound,
rather than accepting any HTTP/infrastructure error.

Convergence is bounded at 20 seconds under explicit one-second TTLs and requires
15 further successful samples over at least 1.5 seconds. Each observation logs
its elapsed time under `--test_arg=-test.v`; this measures polling elapsed time,
not total time since the mutation. The isolated modern-visibility repro uses
**default** app configuration and reproduces both create and update loss in two
independent runs. See `app-org-api-keys-modern-visibility.md`.

Additional frontend inventory: `enterprise/app/api_keys/api_keys.tsx` calls
GetApiKeys/GetApiKey (including includeCertificate); `app/docs/setup_code.tsx`
calls GetBazelConfig and lazy GetApiKey; executor and cache-proxy setup pages use
those same RPCs. `app/auth/certificate_download_link.tsx` creates client-side Blob
URLs from returned certificate contents, not a separate server download RPC.
No API-key-specific streaming/log/raw-JSON read RPC was found. Generic invocation
logs/downloads expose invocation state rather than API-key inventory.

Remaining coverage is explicit: server-admin impersonation creation, enabled certificate generation/mTLS,
distinct developer identities, user-owned-key boundaries, audit-log OLAP reads,
and default-TTL app/proxy convergence are still untested. Thus this remains an
incremental suite, not a claim that every round-2 coverage requirement is met.
The fixture does not expose restart/reconfiguration for a post-bootstrap
server-admin group, nor multiple identities/OLAP. No shared files were edited.

Expiry projections have an additional twice-reproduced default-config bug target,
`expiry_projection_repro`, documented in `app-org-api-keys-expiry-projection.md`.
The green `TestExpiryEnforcement` warms both components with a three-second key,
requires eventual Unauthenticated followed by stable rejection, verifies expired keys
are filtered from detail and inventories (as in the released query).

Round-2 final green targeted invocation:
https://app.buildbuddy.io/invocation/3eb7c0ab-6e24-4c7f-a902-8480568f85cc
Lifecycle polling elapsed 0.229–1.210 seconds; expiry rejection 3.205 seconds,
followed by 15 successful stability samples. These are non-default one-second
TTL results, not default-TTL bounds. Both repro pairs are linked in their finding
files. Ordinary CI excludes the two bug targets. No PR was opened.
