# Usage-alert lifecycle conformance

Released app: v2.314.0, default pinned image
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
The fixture retains actual image inspection metadata when overrides are used.

| Mutation | Read surfaces | Sequence |
| --- | --- | --- |
| CreateUsageAlertingRule | GetUsageAlertingRules and GetUsage over public gRPC and cookie-authenticated UI JSON HTTP | Empty state, distinct configurations, same configuration in different organizations, delete/re-create, second organization |
| DeleteUsageAlertingRule | Same surfaces, both organizations | Delete individual rules while retaining siblings, then empty state |

The model stores complete create responses keyed by server-generated identity.
Every transition compares complete rule sets on both surfaces, including creator
and creation timestamp, configuration and status. Polling uses a bounded deadline
and ticker; lists are compared without depending on ordering. All mutation RPCs
are public gRPC calls with fixture-created ORG_ADMIN keys.

Source inventory (released source in `~/buildbuddy-src`):
- `proto/usage.proto`, `proto/buildbuddy_service.proto`: only create/delete;
  no update or single-rule read. Identical configuration in one organization is rejected by the unique index in `server/tables/tables.go`; delete/re-create generates a fresh identity.
  Repeated delete returns NotFound, so it is not a valid successful operation.
- `enterprise/server/usage_service/usage_service.go`: authenticated organization
  ownership, configuration validation, SQL-backed lists and usage context.
- `enterprise/app/usage/usage_alerts.tsx`: UI uses GetUsageAlertingRules;
  `enterprise/app/usage/usage.tsx`: GetUsage. GetUsage does not embed rules.
- `enterprise/server/usagealert/usagealert.go`: recipients are derived from
  current organization admins; recipients are not writable or readable rule
  fields. Email delivery/evaluator behavior is outside this lifecycle oracle.
- `server/buildbuddy_server/buildbuddy_server.go`: create/delete audit hooks;
  `enterprise/server/auditlog/auditlog.go`: audit reads require configured OLAP.
  This fixture does not configure OLAP, so audit records are not checked.
- Cache proxy server and `cli/`: no usage-alert rule read/write surface.
  GetCurrentBill requires an external billing integration and is excluded.

Run: `bb remote test //tests/statemachine/usage-alerts:go_default_test --test_output=errors`.
