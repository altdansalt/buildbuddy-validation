# Notification state-machine conformance, round 2

The test exercises released enterprise images only. It imports the audit's HTTP
public-key and post-delete consumption checks, retains the original expectations,
and contains no perturbation tests. Every step compares the expected secret
presence/value and audit action history with the applicable surfaces.

| Mutating RPC | Surfaces checked after each step | Expected result |
| --- | --- | --- |
| GetPublicKey (lazy creation) | gRPC with two org API keys; HTTP GetPublicKey; encrypted UpdateSecret followed by notification and executor consumption | Stable nonempty ID and identical valid 32-byte key |
| UpdateSecret | ListSecrets over gRPC with both keys, HTTP (org settings and repo UI backing endpoint), actual `bb secrets list`; Slack webhook; uncached executor `include-secrets` action; gRPC/HTTP GetAuditLogs; full referenced GetInvocation projections | One redacted name, latest value consumed, CREATE then UPDATE audit entries with values redacted; invocation state unchanged |
| DeleteSecret | Same listing, CLI, executor, public-key, audit and invocation surfaces; SendNotification using deleted secret | Empty listing/environment, NotFound and no webhook delivery; key stable; existing audit history unchanged |
| SendNotification | Local webhook path, color, field text/short flag, button type/text/URL/style; all secret listing/key/CLI/executor/audit and invocation surfaces | Exactly one delivery per single-channel request; no persistent secret or invocation mutation |

The deterministic sequence is create, identical repeat, rotate, repeat, rotate,
repeat, delete, re-create, repeat, delete, repeated delete. Two independently
provisioned API keys in the same org alternate writes and consume/read each
other's changes. Notifications test both compare links and parent invocation
links, with repeated valid sends. Every secret check also consumes through the
notification service, including after deletion. Executor actions disable caching
and check the actual injected environment, including absence after deletion.

Listing surfaces poll against the reference model with a 5-second bound. All
consumer/read surfaces are checked again after 250ms. Each delivery is followed
by a 500ms observation interval rejecting late duplicates. Verbose test logs
report observation upper bounds (which include RPC/execution/CLI and observation
latency), rather than claiming to measure an unobserved internal commit time.
BES fixtures settle through bounded polling until the complete HTTP projection
is unchanged for two seconds. gRPC reflection supplies the released invocation
schema; dynamic protobuf decoding compares the **entire** gRPC and HTTP
invocation at baseline and after each step. Capability ordering alone is
normalized as a set. There are no independent-only cross-transport baselines.

## Released source inventory and applicability

- `proto/{notification,secrets,buildbuddy_service}.proto`: notification has only
  SendNotification; there is no notification Get/List, streaming reader, raw
  JSON/download, log, or metadata endpoint exposing notification history.
- `enterprise/app/secrets/{secret_util.ts,secrets_list.tsx,update_secret.tsx}` and
  `enterprise/app/repo/repo.tsx`: GetPublicKey and ListSecrets are the UI read
  surfaces. Edit pages carry the secret name in the URL and do not read values.
  No secret-specific download or streaming read exists.
- `cli/secrets/secrets.go`: actual CLI listing is checked using the runner's
  installed released `bb`. It runs in a disposable Git repo with a fixture-local
  API key in that repo's config and a clean environment. The dispatch key is
  neither read nor inherited by this subprocess. CLI create/update/delete wrap
  the same RPCs; `cli/detect/nondeterminism.go` produces SendNotification and
  exposes no notification-history reader.
- `enterprise/server/{notification/notification.go,secrets/secrets.go}` and
  `server/backends/slack/slack.go`: notification resolves the current org secret
  and posts synchronously. Payload fields beyond the original title/URL checks
  are now asserted, with bounded duplicate observation.
- `enterprise/server/remote_execution/execution_server/execution_server.go`:
  `include-secrets` resolves the same stored secret and injects it in actions;
  this is checked through the released executor. `enterprise/server/gcplink`
  passes ordinary secret variables through this path. Its special GCP refresh
  token exchange concerns a different secret name and requires a real external
  OAuth service, so it is outside this local notification-secret sequence.
- `server/buildbuddy_server/buildbuddy_server.go`,
  `enterprise/server/auditlog/auditlog.go`, `proto/auditlog.proto`, and
  `enterprise/app/auditlogs/auditlogs.tsx`: UpdateSecret logs CREATE/UPDATE;
  DeleteSecret and SendNotification do not log entries in this release. Both
  gRPC and the actual UI HTTP GetAuditLogs endpoint check ordered secret events
  and request-value redaction. Logging is enabled in this test, backed by a
  disposable ClickHouse server started inside the remote test. Its upstream
  24.8.14.39 static archive is SHA512-pinned; it is infrastructure, not a source
  build of BuildBuddy. The helper preserves its log in undeclared outputs.
- `enterprise/server/cmd/cache_proxy/cache_proxy.go`: cache proxy does **not**
  register BuildBuddyService. The suite starts the released proxy and verifies
  ListSecrets/GetPublicKey return Unimplemented. Proxy writes and cross-proxy
  reads/deletes are inapplicable, rather than a convergence contract for this
  state. No requests are sent to a shared deployment.
- `enterprise/server/backends/email/email.go`: notification email uses the fixed
  SendGrid endpoint. The alternate host is an internal constructor option, not
  released configuration. Email cannot be exercised with a test-local sink
  without network/TLS redirection (fault injection) or a product configuration
  change. This is an explicit coverage limit, not a silently skipped test.

Non-default configuration: secret service enabled, disposable local-insecure
KMS master key, audit logging and local OLAP enabled. BuildBuddy app, executor,
and proxy remain the harness-pinned v2.314.0 images:

| Component | Digest (repository is `gcr.io/flame-public/buildbuddy-<component>-enterprise`) |
| --- | --- |
| app | `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` |
| executor | `sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8` |
| proxy | `sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906` |

## Validation

Run only this area:

```sh
bb remote test //tests/statemachine/notifications/... --test_output=all --test_arg=-test.v
```

No confirmed product divergence has been found. Earlier development failures
were an incorrect wire field and ClickHouse fixture pool configuration.

- App/executor/proxy and CLI sequence:
  https://app.buildbuddy.io/invocation/9c18fc14-6c17-4e02-8e28-6f81e1ef70a3
- Audit-enabled sequence with polling and stable rechecks:
  https://app.buildbuddy.io/invocation/6bb9c9f4-9000-4a84-9f13-c13e56c33a78
- Full webhook payload and consumption after every step:
  https://app.buildbuddy.io/invocation/85b7ac57-5070-405c-ac48-6894000dd054

- Full reflection-based invocation projection agreement:
  https://app.buildbuddy.io/invocation/297c9ae0-f253-494d-9bd2-a18a3b27228b
  Observed model-read upper bounds: 0.981–1.130s, including execution, CLI and
  the 500ms no-duplicate observation. Webhook observations: 0.877–2.228ms.
  Stable model rechecks passed after 250ms. These are observation bounds,
  not evidence of a transient divergence.
- Final suite, including CLI auth via disposable Git config:
  https://app.buildbuddy.io/invocation/74c4bcfa-934a-4358-8d54-c0ebdca2fdc5
  Passed in 110s. Model observations: 1.048–1.214s; webhook observations:
  1.596–2.352ms; 250ms stable rereads and 500ms no-duplicate windows passed.

No shared harness/dependency changes are needed. Email coverage would require a
released configurable local email endpoint; no PR is opened for this work.
