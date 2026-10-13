# Organization SSO state conformance

Released app: `gcr.io/flame-public/buildbuddy-app-enterprise:enterprise-v2.314.0`,
pinned to `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
The harness records the actual image for overrides.

| Mutation | Read surfaces | Expected transition |
| --- | --- | --- |
| `SetSSOConfig` | gRPC and UI JSON protolet `GetSSOConfig`; gRPC and UI JSON protolet `GetGroup.ssoEnabled` | Empty → first metadata URL → second metadata URL → empty; repeated sets/clears preserve state; surrounding whitespace is trimmed |

The reference state is the trimmed URL from the last successful mutation. Every
read surface is checked immediately after each acknowledged mutation: the app
updates SQLite synchronously, so there is no asynchronous propagation to poll.
The two documents exercise both accepted SAML metadata root forms. They are
served by a test helper on loopback in the app's network namespace, with an
in-test CA mounted as `SSL_CERT_FILE`. TLS verification remains enabled.
`--http.client.allow_localhost=true` is the released server's explicit test
option allowing this fixture-local destination; normal private-address filtering
otherwise prevents local metadata validation. No external IdP is contacted.

Source inventory at v2.314.0:

- `proto/buildbuddy_service.proto`, `proto/grp.proto`: `SetSSOConfig` is the only
  SSO configuration mutation; `GetSSOConfig` returns the stored URL.
- `server/buildbuddy_server/buildbuddy_server.go`: trims URL whitespace, validates
  HTTPS metadata, synchronously updates the group, and exposes the derived
  `GetGroup.ssoEnabled` flag. `UpdateGroup` preserves existing SSO configuration.
- `enterprise/app/settings/sso_config.tsx`: UI uses the same Get/Set RPCs via
  protolet. The test covers its JSON HTTP read endpoint.
- `enterprise/server/auditlog/auditlog.go`: `UPDATE_SSO_CONFIG` auditing requires
  `app.audit_logs_enabled` and an OLAP database. Default fixture has no ClickHouse;
  audit records are outside this target's reachable surfaces.
- Cache proxy does not register organization SSO RPCs; there is no CLI SSO reader.
  `config.proto.sso_enabled` describes global auth support, not this group's URL.

Run only this area, with a prestarted runner Docker daemon (needed for namespace
inspection):

```
bb remote --runner_exec_properties=init-dockerd=true test //tests/statemachine/sso-config:go_default_test --test_output=errors
```

The helper requires runner `sudo` and `nsenter`; it only serves metadata and does
not start or build a BuildBuddy component. All app requests address the disposable
harness app. Local minimal protobuf wire bindings avoid shared dependency changes.

Verified passing: [Bazel invocation](https://app.buildbuddy.io/invocation/b8ea95e2-0b4a-4fa2-abf3-1010347ab1a2)
([remote runner](https://app.buildbuddy.io/invocation/1ce70bfd-bb93-459d-bd1c-866f5a557888)).
The test creates an organization with `urlIdentifier: sso-conformance` and uses
that public identifier for `GetGroup`; lookup by group ID requires server-admin
access and is not appropriate for an organization-admin conformance test.
