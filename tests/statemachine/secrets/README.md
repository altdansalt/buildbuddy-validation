# Encrypted secret state machine (released v2.314.0)

Target: `//tests/statemachine/secrets:go_default_test`.

| Mutating RPC | Surfaces compared with the model after every step, twice | Result |
| --- | --- | --- |
| GetPublicKey (lazy key creation) | Two same-org API-key gRPC clients, cookie-authenticated JSON and frontend prefixed-protobuf GetPublicKey; encrypted writes and executor decryption | PASS (covered surfaces) |
| UpdateSecret | Exact redacted ListSecrets via both gRPC clients, HTTP JSON, default frontend application/proto+prefixed, bb secrets list; stable public keys; uncached full and filtered executor injection with SHA256 and set/unset markers; Slack notification resolution to local webhook | PASS (covered surfaces) |
| DeleteSecret | All the same surfaces; deleted variables must be unset; notification resolution must return NotFound after deleting its referenced secret | PASS (covered surfaces) |

The audit probe is integrated into the main test with original expectations.
No perturbation tests are retained. JSON missing secret fields are permitted
only for an empty model (protobuf JSON omits default repeated fields); an
explicit non-array field always fails, as do unexpected response fields.

The sequence covers independent creation, two replacements, encrypted empty
plaintext, repeated writes, deletion, repeated deletion, recreation, and final
cleanup. Writes alternate between two ORG_ADMIN keys in the same organization and
the frontend cookie-authenticated prefixed-protobuf mutation endpoint;
both clients read after every step. ALPHA_TOKEN is a fixture-local webhook URL:
changing it must change the observed delivery path, with no real Slack traffic.
Each entire observation is repeated after both execution consumers and the
notification consumer to assert stable state. Every action disables cache lookup
and storage. No plaintext leaves the execution consumer, only presence markers
and hashes. The local webhook receives notification payloads, not secret values.

Transactions are synchronous. Immediate mismatches fail rather than being
hidden by polling. Logs report elapsed time from write dispatch to the first
complete successful observation, including two execution actions and notification
delivery; this is an observation upper bound, **not measured replication lag**.
A second full observation checks stability. RPCs/actions have bounded contexts.

## Source inventory and remaining integration coverage

Inspected the v2.314.0 checkout under `~/buildbuddy-src`:

- `proto/secrets.proto`, `proto/buildbuddy_service.proto`: four secret RPCs,
  no plaintext GetSecret, download, raw JSON, metadata or streaming secret RPC.
- `enterprise/server/secrets/secrets.go`: synchronous encrypted SQL storage,
  list redaction and filtered GetSecretEnvVars.
- `server/buildbuddy_server/buildbuddy_server.go`: UpdateSecret CREATE/UPDATE
  audit hook; DeleteSecret has no audit hook. This is source evidence only,
  **not a twice-reproduced runtime finding**.
- `enterprise/app/secrets/{secret_util.ts,secrets_list.tsx}` and
  `enterprise/app/repo/repo.tsx`: UI GetPublicKey, UpdateSecret, DeleteSecret,
  ListSecrets; repo configuration also reads ListSecrets. `app/service/rpc_service.ts`
  and `server/static/static.go` select prefixed protobuf by default. Tested the
  actual endpoint/framing, message flags, lengths and gRPC status.
- `cli/secrets/secrets.go`: list is the only CLI read command. Tests require the
  runner-installed bb; the binary is not release-pinned.
- `enterprise/server/remote_execution/execution_server/execution_server.go`:
  full/filtered injection, including frontend AI-agent env-secrets use.
- `enterprise/server/workflow/service/service.go`: trusted workflows set
  include-secrets=true and use the tested execution injection path. End-to-end
  workflow creation, GitHub integration, checkout and CI runner are **not tested**.
- `enterprise/server/notification/notification.go` and `proto/notification.proto`:
  SendNotification resolves a named secret on every request. Tested against a
  local HTTP receiver for creation, replacement, persistence and deletion.
- `enterprise/server/gcplink/gcplink.go`: CLOUDSDK_AUTH_REFRESH_TOKEN exchange
  and GetGCPProject use fixed accounts.google.com / cloudresourcemanager URLs.
  **Not tested**: valid linked GCP credentials and fixture-local Google endpoints
  are unavailable. Invalid tokens would violate valid-input-only scope; no such
  probe is used. These paths must not be claimed as conformance coverage.
- `enterprise/server/auditlog/auditlog.go`: audit reads require configured
  ClickHouse plus app.audit_logs_enabled. **Not tested**: the shared harness has
  no OLAP fixture. Coordinator follow-up: a digest-pinned local ClickHouse
  fixture and audit read/write convergence tests over gRPC and UI transport.
- `enterprise/server/cmd/cache_proxy/cache_proxy.go`: no BuildBuddyService or
  secret storage/service registration. Proxy secret writes, deletes, reads and
  secret replication are inapplicable; starting a proxy cannot add this surface.

This closes frontend transport, filtered injection, variable-presence and
notification gaps. It does **not** claim exhaustive audit/GCP/workflow integration
coverage. No confirmed divergence or finding file is claimed.

## Fixture and images

Random test-local AES master key; supported development local KMS; local
SQLite/Redis and bare executor. The newly created fixture bridge address lets the app reach the
local webhook on the runner without a default Docker bridge. No shared deployment requests, external integration
credentials, fault injection or component builds are used.

- App: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- Executor: `gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8`

Actual images, configs and logs are preserved by the harness.

Validation command:

```sh
bb remote test //tests/statemachine/secrets:go_default_test --test_output=all --test_arg=-test.v --nocache_test_results
```

Final expanded-suite validation (includes frontend mutations):
https://app.buildbuddy.io/invocation/cd864826-2b12-4ede-990a-91bf839666ee

All 12 transitions passed; first complete observations took 247–276 ms and
second complete observations stayed stable (493–550 ms from write dispatch).
These bounds include consumer runtime, not a demonstrated transient divergence.

Earlier expanded gRPC-only mutation run also passed:
https://app.buildbuddy.io/invocation/3ed41d19-6707-4991-81ec-cd4d33fb51e8
