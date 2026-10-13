# Notification delivery conformance (v2.314.0)

Released source inventory:

| Mutating RPC | Read surfaces / observable effect | Coverage |
| --- | --- | --- |
| SendNotification | No notification Get/List RPC; local Slack webhook message; GetInvocation gRPC and UI JSON API for referenced state | Two valid build IDs, with/without parent, repeated sends, secret rotation; exactly one captured message per successful single-channel request; all referenced invocation snapshots unchanged |
| UpdateSecret (channel prerequisite) | ListSecrets gRPC and UI JSON API; next webhook delivery | Create, identical repeat, rotate; names visible, values redacted, new sink path selected |
| DeleteSecret (channel prerequisite) | ListSecrets gRPC and UI JSON API | Delete and repeated delete; referenced builds unchanged |
| GetPublicKey (lazy key creation prerequisite) | Returned group public key | Used to seal fixture-local webhook URL with NaCl anonymous sealed box |

Sources: proto/notification.proto, proto/secrets.proto,
proto/buildbuddy_service.proto, enterprise/server/notification/notification.go,
enterprise/server/secrets/secrets.go, enterprise/server/backends/kms/kms.go,
enterprise/server/backends/email/email.go, server/backends/slack/slack.go,
cli/secrets/secrets.go, cli/remotebazel/remotebazel.go, and cache proxy service
registration. No notification state is exposed by the cache proxy or CLI.
The CLI's `bb secrets list` wraps ListSecrets; its executable is not a pinned
harness dependency, so this test checks the backing RPC but does not invoke it.

The supported local-insecure KMS configuration is for disposable test keys only.
The test uses harness-pinned app image
`gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`.
No executor or proxy is needed for notification service state. Invocation
fixtures are published through local BES and finalized before snapshots.
Readiness is bounded polling; delivery waits on a channel with a bounded context.

Email channel is not exercised: the released registration uses the default
SendGrid host, and the injectable SendGridHost option is an internal Go client
constructor option, not a released server flag. A fixture-local email sink would
require a harness-supported network/TLS redirect or a product configuration
option. No email provider credentials or real recipients are used.

Invocation comparisons normalize `created_with_capabilities` ordering because
that projection represents a set. All other wire/UI fields remain in the
snapshots. The local sink binds a runner port and is addressed by the runner's
private IPv4 address (not a DNS alias); it is closed during test cleanup.

Validation: `bb remote test //tests/statemachine/notifications:go_default_test
--test_output=errors` passed:
https://app.buildbuddy.io/invocation/93b3f432-01d2-40d3-b9e4-529828e25f4c
Runner: https://app.buildbuddy.io/invocation/9bfaffa5-8687-4ef3-929b-4ae493060877
Earlier test-development runs exposed set ordering and a runner DNS alias
configuration issue; neither is classified as a product defect.
