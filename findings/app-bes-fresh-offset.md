# Fresh BES streams starting above one are ACKed without persistence

Title: Fresh BES streams starting above one are ACKed without persistence
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: An authenticated uploader that loses its initial event or incorrectly resumes a new invocation receives successful commit acknowledgements for discarded data, suppressing retries and hiding missing build records. This requires a nonstandard or interrupted client; no cross-organization impact was demonstrated.
Status: unverified
Repro test target: //tests/app/bes:fresh_offset
BuildBuddy invocation: https://app.buildbuddy.io/invocation/d375f6f1-7f04-4f31-9c9d-578fa786679c

## Expected behavior

The released [OrderedBuildEvent contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L150-L159) says sequence numbers are consecutive natural numbers starting at one. The response identifies an event that has been committed. A brand-new UUID with no earlier events cannot be a suffix retry of an already committed invocation. Reject the invalid initial sequence without ACKing it, or actually retain it with an explicit supported recovery policy.

Suffix retries of completed invocations should remain supported; `regression/complete_and_retry` covers that distinct case.

## Actual behavior

Sending one valid Bazel BuildStarted payload at sequence 2 for a fresh UUID returns an ACK for sequence 2 and successful stream completion. The second reproduction also queries GetInvocation immediately after the ACK: the app returns NotFound.

Reproduced in independent fixture-local containers on [run 1](https://app.buildbuddy.io/invocation/d375f6f1-7f04-4f31-9c9d-578fa786679c) and [run 2](https://app.buildbuddy.io/invocation/981c7e26-28bc-4eb4-9689-01ebe8313cbf). Both bug tests failed at the intended assertion. The second run first ACKed a valid sequence-1 control using the same client and fixture. [Regression coverage](https://app.buildbuddy.io/invocation/7c914798-1c87-492b-86e2-9a7ae6bef94a) also uploads, retries, and reads back a successful completed invocation. This rules out auth, protobuf encoding, readiness, and storage setup as explanations for the offset behavior.

## Logs excerpt

```text
ACKed invocation readback: result=map[] error=GetInvocation HTTP 500: rpc error: code = NotFound desc = invocation not found
fresh stream starting at 2 must be rejected without ACKs:
ACKs=[stream_id:{build_id:"19c73a05-8e5a-4648-8a67-420b37680296" invocation_id:"19c73a05-8e5a-4648-8a67-420b37680296" component:TOOL} sequence_number:2] error=<nil>

Third run container log (new UUID):
Voiding EventChannel for invocation d4a5ac88-0ae9-480d-a911-da56520ca467: build event stream starts with sequence number > 1 (2), which likely means Bazel is retrying an invocation that we already finalized.
```

## Notes

No image overrides, custom flags, or config overrides. Harness defaults: test-local SQLite, disk storage, Redis, built-in self-auth, and a disposable local API key. Only app and its local Redis sidecar start. Full container logs/config/inspect data are preserved in undeclared outputs.

The released [handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_handler/build_event_handler.go#L999-L1017) sets `isVoid` for any initial sequence greater than one, without establishing whether that invocation was previously committed. Finalization is a no-op for void channels, while the server still sends ACKs. The source comment explicitly acknowledges relying on clients always starting at one. An appropriate fix could verify persisted completion before allowing this suffix retry path.

Reproduce with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:fresh_offset --test_tag_filters=bug --nocache_test_results` after configuring the public runner fetch remote per AGENTS.md. The Remote Execution API does not specify BES; this finding uses the release's google.devtools.build.v1 contract. Status is left unverified for maintainer triage as requested; no upstream report was sent.

Final validation with preserved-container panic checks: [green regression](https://app.buildbuddy.io/invocation/f3221279-bdb0-4c57-a375-ec1522d91113), [third explicit reproduction](https://app.buildbuddy.io/invocation/09b89765-610b-46a7-8bba-b0a493a04b78). Both reproducers failed at the intended assertion again; no panic/fatal-error markers were detected.
