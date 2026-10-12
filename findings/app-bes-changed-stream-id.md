# BES accepts a changed stream identity and ACKs the wrong invocation

Title: BES accepts a changed stream identity and ACKs the wrong invocation
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: A misrouted or multiplexed authenticated event stream can receive successful commit ACKs naming a different invocation, permitting silent event mixing and misleading build records. This reproduction uses two disposable invocation IDs under the same local credentials; it does not demonstrate access to another organization's invocation.
Status: unverified
Repro test target: //tests/app/bes:changed_identity
BuildBuddy invocation: https://app.buildbuddy.io/invocation/d375f6f1-7f04-4f31-9c9d-578fa786679c

## Expected behavior

[PublishBuildToolEventStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L72-L75) publishes events belonging to the same stream. Each OrderedBuildEvent identifies its stream, and each ACK identifies the stream containing the committed event. Reject a change of stream identity within an RPC. In particular, do not ACK an event for invocation B as an event for invocation A.

## Actual behavior

One RPC sends BuildStarted for invocation A at sequence 1, then BuildFinished for a distinct invocation B at sequence 2. Both invocation_id and build_id change. The server completes successfully and emits ACKs for sequences 1 and 2, both with A's StreamId. No event for A at sequence 2 was sent.

Reproduced independently in [run 1](https://app.buildbuddy.io/invocation/d375f6f1-7f04-4f31-9c9d-578fa786679c) and [run 2](https://app.buildbuddy.io/invocation/981c7e26-28bc-4eb4-9689-01ebe8313cbf). Both tests failed on the intended successful-completion assertion. The second run includes a valid sequence-1 control in the same fixture before the mixed-ID RPC. The same encoding/client/config also passes [normal upload, retry, and readback coverage](https://app.buildbuddy.io/invocation/7c914798-1c87-492b-86e2-9a7ae6bef94a), ruling out fixture/auth/encoding issues. The generated BES client serializes the two explicitly supplied IDs; the harness attaches only a local API key and does not rewrite requests.

## Logs excerpt

```text
RPC containing distinct streams must be rejected:
sent invocation IDs=[f25833d7-29c5-44e2-89b9-9c3b88c73bf6,77d44b08-def2-4d33-a053-e1388a7c71b8]
ACKs=[stream_id:{build_id:"f25833d7-29c5-44e2-89b9-9c3b88c73bf6" invocation_id:"f25833d7-29c5-44e2-89b9-9c3b88c73bf6" component:TOOL} sequence_number:1
      stream_id:{build_id:"f25833d7-29c5-44e2-89b9-9c3b88c73bf6" invocation_id:"f25833d7-29c5-44e2-89b9-9c3b88c73bf6" component:TOOL} sequence_number:2]
error=<nil>
```

## Notes

No custom image, flags, or config. The default app fixture provides SQLite/local storage, Redis, built-in self-auth, and a disposable API key. No shared service receives component requests. Full configs, logs, and inspect metadata are preserved in undeclared outputs.

The released [server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go) opens its channel and saves StreamId only on the first message. Later messages go to that same channel without an identity equality check. `postProcessStream` emits every ACK with the first StreamId. Comparing all subsequent StreamIds against the first before HandleEvent would avoid this inconsistency. Persistence contents and cross-organization behavior are not asserted by this minimal reproducer.

Reproduce with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:changed_identity --test_tag_filters=bug --nocache_test_results` with the public fetch remote configured per AGENTS.md. The Remote Execution API does not define BES; this uses the pinned release's BES service contract. Status remains unverified for maintainer triage as requested. No upstream report was sent.

Final validation with preserved-container panic checks: [green regression](https://app.buildbuddy.io/invocation/f3221279-bdb0-4c57-a375-ec1522d91113), [third explicit reproduction](https://app.buildbuddy.io/invocation/09b89765-610b-46a7-8bba-b0a493a04b78). Both reproducers failed at the intended assertion again; no panic/fatal-error markers were detected.
