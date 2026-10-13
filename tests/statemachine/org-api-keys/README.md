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
