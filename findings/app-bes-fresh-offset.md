# Fresh BES streams starting above one are ACKed without persistence

Title: Fresh BES streams starting above one are ACKed without persistence
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Malformed authenticated clients receive misleading successful commit acknowledgements. The repro does not demonstrate impact on conforming clients or a cross-organization authorization bypass; independent review therefore lowers severity from medium to low.
Status: confirmed
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

Reproduce with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:fresh_offset --test_tag_filters=bug --nocache_test_results` after configuring the public runner fetch remote per AGENTS.md. The Remote Execution API does not specify BES; this finding uses the release's google.devtools.build.v1 contract. The original report left status unverified pending independent review; no upstream report was sent.

Final validation with preserved-container panic checks: [green regression](https://app.buildbuddy.io/invocation/f3221279-bdb0-4c57-a375-ec1522d91113), [third explicit reproduction](https://app.buildbuddy.io/invocation/09b89765-610b-46a7-8bba-b0a493a04b78). Both reproducers failed at the intended assertion again; no panic/fatal-error markers were detected.

## Verification

Verdict: **confirmed**. The client is invalid: a fresh stream must begin at 1. The defect is the successful commit acknowledgement for data deliberately discarded, not the lack of support for starting at 2. The response contract explicitly says failure to commit causes an RPC error. Legitimate suffix retries can be ACKed when previously committed; this test uses a new random UUID and rules that case out.

The [handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_handler/build_event_handler.go#L999-L1017) intentionally assumes every initial sequence greater than 1 is a finalized-invocation retry, sets `isVoid`, and performs no persisted-completion lookup. [FinalizeInvocation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_handler/build_event_handler.go#L852-L856) becomes a no-op, while [postProcessStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L229-L266) ACKs sequence 2. Both runs returned that ACK and nil error, then GetInvocation returned NotFound. This source comment explains the optimization but does not make a nonexistent commit a valid acknowledgement.

Severity changed from medium to **low**: the demonstrated loss requires an uploader violating the initial-sequence contract. No failure of conforming first attempts or loss of previously committed suffix retries was shown. The test and central claim are correct; an interrupted upload alone is not sufficient to establish this defect without the malformed fresh-stream condition.

Reviewed independently on 2026-10-12 against PR #22's tests and the exact app digest above. No image, flag, or fixture configuration overrides were used. RPCs address the fixture's loopback container endpoints with a disposable local API key. The request helpers encode the pinned Bazel protobuf fields and do not rewrite stream identities. Each reproducer first successfully ACKs a sequence-1 control; the full BES regression target also passed in the first verification invocation, including completed invocation readback and legitimate suffix retries. Both independent uncached executions failed at the reported assertion, without a panic marker; this is deterministic behavior rather than readiness, authentication, or storage flakiness.

Checked [bazelbuild/remote-apis](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto): REAPI defines execution/cache APIs and invocation correlation in RequestMetadata, not BES acknowledgement semantics. The applicable contract is the release's [PublishBuildEvent proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto#L138-L159). The release's [write architecture docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/architecture-build-event-write.md) do not document an exception to that contract.

Commands used `GIT_CONFIG_GLOBAL=/dev/null bb remote test <targets> --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`, with the public GitHub fetch remote. [Verification run 1](https://app.buildbuddy.io/invocation/3f50e147-6d05-4e95-9dce-28d0ec43b8a6) ran both bug targets and `//tests/app/bes:regression`; [verification run 2](https://app.buildbuddy.io/invocation/65e0f9cf-fbd6-4f3b-bb1f-8869d57f4233) ran both bug targets. Failures are the expected defect assertions, not build failures.

## Version history

Tested on 2026-10-13 against five released linux/amd64 app images, spanning
2026-04-16 through 2026-10-08. The unchanged bug-tagged target
`//tests/app/bes:fresh_offset` from PR branch
`area/app-bes` at `f13831aec05f0a4204f08487ce7af2c0278dcdff` ran once per release
with fresh fixture-local containers and test-result caching disabled.

| Enterprise release | Release date | GCR manifest digest | Result | BuildBuddy invocation |
| --- | --- | --- | --- | --- |
| 2.314.0 | 2026-10-08 | `sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/73a8336f-ad5e-4632-9b3e-6b1ef362ab2c) ([runner](https://app.buildbuddy.io/invocation/52f381ca-2e20-41b8-9f5e-7d991d8ba75d)) |
| 2.313.0 | 2026-10-06 | `sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/027fd6f9-3a36-4245-a90f-d5bba1be228c) ([runner](https://app.buildbuddy.io/invocation/96568462-b6c8-4205-a1ca-14d2c289c481)) |
| 2.300.0 | 2026-09-01 | `sha256:03bd9ccb5888936beb301a577bf01560511cded6fa909079cbb53277aa32ed68` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/d6058197-e260-4db6-8838-7bead66fc4c3) ([runner](https://app.buildbuddy.io/invocation/88cbc43b-f2bc-4291-8c13-005e1eca9327)) |
| 2.280.0 | 2026-06-23 | `sha256:d9fbae912f786a0f95d351ffb7c580a6ecb8b1fb636fd3fe8899bf1cb8350a35` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/928050db-890c-4b7b-a451-f01568a743e8) ([runner](https://app.buildbuddy.io/invocation/4305fb17-c221-4a2c-ad29-45207066821c)) |
| 2.260.0 | 2026-04-16 | `sha256:0f6376865cc9c92d0e837aa1cb53717b59875db68a1b09b6dd479c571f6ab88e` | Reproduces | [Bazel results](https://app.buildbuddy.io/invocation/5a3c4c84-24f5-4e03-bd53-6baaf85cad67) ([runner](https://app.buildbuddy.io/invocation/dfd702a3-f796-4d22-9a7a-cfdba85df0a5)) |

All image references use
`gcr.io/flame-public/buildbuddy-app-enterprise@<digest>`; digests were resolved
from the GCR manifest API for the corresponding `enterprise-v<version>` tags.
The 2.314.0 digest matches the repository default.

Observed on every tested version: A fresh stream starting at sequence 2 succeeds and ACKs sequence 2 even though invocation readback is NOT_FOUND; container logs say the EventChannel was voided.

**Earliest tested release reproducing: 2.260.0 (2026-04-16).**
The defect was present by that release; the introduction version is unknown.
All sampled releases reproduce, so this matrix does not establish an
introduction interval or imply every intervening release was tested.

No older release required startup configuration changes. All five reached
the intended product assertion after fixture startup and authentication.

Command pattern (run from the repro branch; substitute the digest above):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote --runner_exec_properties=init-dockerd=true test \
  //tests/app/bes:fresh_offset \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image=gcr.io/flame-public/buildbuddy-app-enterprise@<digest>
```
