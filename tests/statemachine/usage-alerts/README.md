# Usage-alert lifecycle conformance

Released app v2.314.0:
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.

| Mutating RPC | Surfaces checked after every step | Result |
| --- | --- | --- |
| CreateUsageAlertingRule | GetUsageAlertingRules, GetAuditLogs: app public gRPC with two independent org-admin keys; UI JSON, binary protobuf, framed protobuf HTTP. GetUsage transport equality. Both organizations. | Passing |
| DeleteUsageAlertingRule | Same surfaces, including retained audit history and empty rule sets | Passing |

The deterministic sequence creates distinct configurations and identical
configurations in different organizations, deletes siblings independently,
re-creates a deleted configuration with a fresh ID, and repeats three valid
create/delete cycles. The peer key writes and the primary key deletes. Every
step checks both organizations, including their complete rule sets and audit
request multisets. Audit pagination is followed. Full audit entries agree across
all transports, including event/authentication/resource metadata. Independent
checks verify request configuration, creator absence for organization keys,
creation time bounds, initial empty status, audit action, key label and event
time bounds. Server-assigned IDs and creation timestamps remain captured values;
these cannot be predicted exactly, but are independently validated. Duplicate
rule IDs fail. A present non-array list fails; omitted repeated fields are legal
protobuf JSON encodings of an empty list and are still compared with the model.

Each surface must converge within ten seconds and remain correct for three
samples spaced 100ms apart. Verbose output records read latency and elapsed time
since the mutation for each surface (0 primary gRPC, 1 peer gRPC, 2 JSON,
3 binary, 4 framed). Sequential surface observations are upper bounds on
convergence, not measurements of replication delay. GetUsage contains no rule
state and is checked only as usage-page context.

Audit coverage uses non-default `app.audit_logs_enabled=true` and fixture-local
ClickHouse 24.8.14.39, pinned to
`mirror.gcr.io/clickhouse/clickhouse-server@sha256:1ffa82edee000a42c09313bd9f1293d94c570aee74babc1b3ca9983a35fa597b`.
The container starts inside the test, uses an ephemeral published port, and is
removed during cleanup; logs are saved in undeclared outputs. App usage tracking
and alerts are enabled, but no alert evaluator/email delivery job is started.

## Released-source inventory

- `proto/usage.proto`, `proto/buildbuddy_service.proto`: Create/Delete are the
  only mutations; no Update, single-rule read, or stream exists. The SQL unique
  index rejects duplicate configurations in one organization; repeated delete
  is NotFound. These unsuccessful requests are outside valid-operation scope.
- `enterprise/server/usage_service/usage_service.go`: SQL rules and GetUsage;
  CSV usage export contains aggregate usage, never alert rules.
- `enterprise/app/usage/usage_alerts.tsx`: GetUsageAlertingRules, Create, Delete.
  `enterprise/app/usage/usage.tsx`: GetUsage context.
- `app/service/rpc_service.ts`, `server/util/protolet`: binary and framed
  protobuf HTTP transports. Both are exercised along with JSON.
- `server/buildbuddy_server/buildbuddy_server.go`: Create/Delete audit hooks;
  `enterprise/server/auditlog/auditlog.go` and
  `enterprise/app/auditlogs/auditlogs.tsx`: GetAuditLogs with explicit timestamp
  bounds and pagination. The initial trial accidentally omitted the UI's date
  bounds and correctly failed; that was a test configuration error.
- `server/static/static.go`: usage feature flags are configuration, not rule
  state. No downloads, raw JSON views, metadata endpoint or streaming RPC
  exposes another rule projection. `enterprise/server/usagealert/usagealert.go`
  derives email recipients from org admins; no mutable rule recipient field.
- Released cache proxy service registration and `cli/`: no alert-rule read/write
  surface. Proxy writes/deletes and proxy agreement are inapplicable. Billing
  integration, usage CSV and cache download APIs expose no alert-rule state.

Run (prestarted runner Docker daemon required for the OLAP sidecar):

```sh
bb remote --runner_exec_properties=init-dockerd=true test //tests/statemachine/usage-alerts:go_default_test --test_output=all --test_arg=-test.v --nocache_test_results
```

No product divergence has been observed. Validation links and observed timings
are recorded below after the final run.

Expanded lifecycle including OLAP audit requests passed:
https://app.buildbuddy.io/invocation/eba479c3-1ffc-431f-abc6-3eb0bafe4b63

Full audit metadata/transport agreement and verbose convergence measurements passed:
https://app.buildbuddy.io/invocation/af4fa1f2-4122-4d8f-9ff3-ecae1179271a
Rule read latency was at most 4.36ms; audit read latency at most 9.95ms.
All sequential surface observations completed within 4.05s of mutation,
including the intentional stability delays. No transient mismatch was observed.
Each convergence result remained correct for at least another 200ms.

Final version, including strict stability-deadline failure, passed in 84.5s:
https://app.buildbuddy.io/invocation/b9f410b9-9a19-462b-aaa6-68445e0c0295
Runner: https://app.buildbuddy.io/invocation/2d9806ef-3147-4ae1-a652-6983e46a2e53
170 rule and 170 audit first observations; all were correct immediately.
Maximum rule read 6.14ms, audit read 15.73ms; all sequential observations
within 4.12s of mutation, then stable for two additional samples (200ms).
No confirmed divergence, so no bug target or findings file is warranted.
