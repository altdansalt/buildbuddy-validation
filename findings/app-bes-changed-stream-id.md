# BES accepts a changed stream identity and ACKs the wrong invocation

Title: BES accepts a changed stream identity and ACKs the wrong invocation
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Malformed authenticated clients receive misleading successful commit acknowledgements. The repro does not demonstrate impact on conforming clients or a cross-organization authorization bypass; independent review therefore lowers severity from medium to low.
Status: confirmed
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

Reproduce with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:changed_identity --test_tag_filters=bug --nocache_test_results` with the public fetch remote configured per AGENTS.md. The Remote Execution API does not define BES; this uses the pinned release's BES service contract. The original report left status unverified pending independent review. No upstream report was sent.

Final validation with preserved-container panic checks: [green regression](https://app.buildbuddy.io/invocation/f3221279-bdb0-4c57-a375-ec1522d91113), [third explicit reproduction](https://app.buildbuddy.io/invocation/09b89765-610b-46a7-8bba-b0a493a04b78). Both reproducers failed at the intended assertion again; no panic/fatal-error markers were detected.

## Verification

Verdict: **confirmed**. Sending two identities in one RPC violates the documented same-stream request contract. That does not justify an ACK claiming that A/2 was committed when the client sent B/2. The test correctly expects rejection of the conflicting identity, and both runs instead succeeded with two ACKs naming only A. This is an input-validation and acknowledgement-integrity defect; persisted event mixing and cross-organization effects remain unproven.

The [server receive loop](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L188-L209) stores and opens a channel using only the first StreamId and never compares subsequent StreamIds. [postProcessStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L229-L266) then uses that first ID for every ACK. The [service contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L72-L75) requires one stream, and the response identifies the stream containing the committed event. No documented multiplexing policy was found that could make rewriting B to A valid.

Severity changed from medium to **low**: only a malformed authenticated uploader is demonstrated. Ordinary single-identity clients are unaffected by this repro, and it establishes neither an authorization bypass nor persisted cross-invocation corruption.

Reviewed independently on 2026-10-12 against PR #22's tests and the exact app digest above. No image, flag, or fixture configuration overrides were used. RPCs address the fixture's loopback container endpoints with a disposable local API key. The request helpers encode the pinned Bazel protobuf fields and do not rewrite stream identities. Each reproducer first successfully ACKs a sequence-1 control; the full BES regression target also passed in the first verification invocation, including completed invocation readback and legitimate suffix retries. Both independent uncached executions failed at the reported assertion, without a panic marker; this is deterministic behavior rather than readiness, authentication, or storage flakiness.

Checked [bazelbuild/remote-apis](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto): REAPI defines execution/cache APIs and invocation correlation in RequestMetadata, not BES acknowledgement semantics. The applicable contract is the release's [PublishBuildEvent proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L138-L159). The release's [write architecture docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/architecture-build-event-write.md) do not document an exception to that contract.

Commands used `GIT_CONFIG_GLOBAL=/dev/null bb remote test <targets> --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`, with the public GitHub fetch remote. [Verification run 1](https://app.buildbuddy.io/invocation/3f50e147-6d05-4e95-9dce-28d0ec43b8a6) ran both bug targets and `//tests/app/bes:regression`; [verification run 2](https://app.buildbuddy.io/invocation/65e0f9cf-fbd6-4f3b-bb1f-8869d57f4233) ran both bug targets. Failures are the expected defect assertions, not build failures.
