# Invalid BES sequences return Unknown instead of InvalidArgument

Title: Invalid BES sequences return Unknown instead of InvalidArgument
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: An authenticated malformed uploader receives an ambiguous error classification rather than an actionable input error. No lost acknowledged data, ordinary-client failure, authorization bypass, or server crash is demonstrated.
Status: rejected
Repro test target: //tests/app/bes:invalid_sequence_code
BuildBuddy invocation: https://app.buildbuddy.io/invocation/93ffaa2e-bf0a-4b57-89d2-cca06d34c301

## Expected behavior

The [released BES contract](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto) requires consecutive sequence numbers. When a stream contains an unfilled gap or duplicate, classify the invalid request as `InvalidArgument`. The [gRPC status guide](https://grpc.io/docs/guides/status-codes/) defines this code for invalid arguments independently of system state, while `Unknown` describes an unknown error, such as insufficient error information. The gap and duplicate are fully identifiable from the request itself.

This is a proposed canonical classification, not a claim that the BES proto explicitly mandates a particular status code. Returning an error and withholding ACKs already satisfies the core commit contract.

## Actual behavior

A stream with sequences `[1, 3]` or `[1, 1]` returns `Unknown` with an explicit sequence-mismatch description and no ACKs. This reproduced in [run 1](https://app.buildbuddy.io/invocation/93ffaa2e-bf0a-4b57-89d2-cca06d34c301) and [run 2](https://app.buildbuddy.io/invocation/099da032-0bf8-44de-85d8-4134eda6c41f), each in an independent fixture-local app container with uncached tests. Both fail only the intended status-code assertions.

Each repro first successfully publishes and ACKs a complete sequence-1/2 control with the same client, authentication and payload helpers. Round-2 normal upload/retry/size-boundary coverage also passes in run 2. This rules out readiness, auth, protobuf encoding and fixture configuration as causes. Container logs are checked for panic and fatal-error markers.

## Logs excerpt

```text
sequence=[1 3] ACKs=[] code=Unknown error=rpc error: code = Unknown desc = event sequence number mismatch: received 3, wanted 2; want InvalidArgument
sequence=[1 1] ACKs=[] code=Unknown error=rpc error: code = Unknown desc = event sequence number mismatch: received 1, wanted 2; want InvalidArgument
```

## Notes

No image, config or flag overrides. Default fixture-local SQLite/disk storage, Redis, built-in self-auth and disposable local API key. All component requests use fixture endpoints. Full configs, inspect metadata and logs are preserved in test undeclared outputs. No upstream report was sent.

The released [postProcessStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L229-L246) explicitly constructs `status.UnknownErrorf` when sorted sequence numbers are nonconsecutive. Changing this classification should preserve rejection and the absence of commit ACKs. Independent maintainer review should decide whether the intentional retry behavior warrants retaining `Unknown`; the independent verification below rejects the proposed classification defect.

Reproduce: `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:invalid_sequence_code --test_tag_filters=bug --nocache_test_results --test_output=all` with the public runner fetch remote configured per AGENTS.md.

## Verification

Verdict: **rejected** (independent review, 2026-10-13). Reviewed PR #87 at
`5e61ba80376f446685e9b365e902d1670ca26de6`, including `codes_test.go`,
`bes_test.go`, the BUILD target and fixture setup. The observed behavior is real
and deterministic, but the test's requirement for InvalidArgument is unsupported
by the BES contract and conflicts with the intended retransmission behavior.

Command for both independent, uncached hosted-runner executions:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:invalid_sequence_code --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Run 1: test invocation](https://app.buildbuddy.io/invocation/838723f8-3f8d-4837-a80d-1f8b9096c0fc), [runner invocation](https://app.buildbuddy.io/invocation/5b1b2920-4f4a-4371-90ae-63bb7076e00c).
- [Run 2: test invocation](https://app.buildbuddy.io/invocation/4050cc89-53d2-4e8b-9dab-f745d496a28a), [runner invocation](https://app.buildbuddy.io/invocation/4d1bbf32-a086-4956-a2c7-60f1b6146e6e).

Both runs passed the complete sequence-1/2 control, then returned Unknown with
zero ACKs for both `[1, 3]` and `[1, 1]`. Neither showed fixture/auth failure,
panic or a timeout. The exact app image/digest above was used without overrides.
The valid control and precise server error rule out payload encoding, readiness
and test flakiness as explanations for the observation.

The [released BES proto](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/publish_build_event.proto)
says sequence numbers should be consecutive and failures to commit cause RPC
errors; it does not prescribe InvalidArgument for this stream condition.
REAPI's Execution/WaitExecution error contract in bazelbuild/remote-apis applies
to a different service and cannot impose a BES error classification. The
[gRPC status guide](https://grpc.io/docs/guides/status-codes/) makes
InvalidArgument a plausible design choice, but does not establish a BES-specific
requirement or demonstrated correctness failure here.

More decisively, [postProcessStream in v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L222-L245)
explicitly says missing consecutive events must withhold *all* ACKs and force
retransmission to avoid cross-server consistency problems, and deliberately
returns Unknown. [Bazel 8.0.0's uploader](https://github.com/bazelbuild/bazel/blob/8.0.0/src/main/java/com/google/devtools/build/lib/buildeventservice/BuildEventServiceUploader.java#L722-L728)
retries every status except InvalidArgument; [its stream retry loop](https://github.com/bazelbuild/bazel/blob/8.0.0/src/main/java/com/google/devtools/build/lib/buildeventservice/BuildEventServiceUploader.java#L538-L580)
resends unacknowledged events. Changing the code as proposed would stop that
recovery. This source evidence establishes intent rather than merely inferring
it from the current implementation. The released [build-event write documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/architecture-build-event-write.md)
provides an architecture overview, without a stronger status-code promise.

The test correctly observes rejection and absent ACKs, but its extra status-code
assertion is not a conformance oracle. Retain coverage that malformed sequences
produce an error without ACKs; remove the bug classification unless a separate
client failure or stronger contract is demonstrated. Severity **low** is retained
as the original hypothetical impact, not an accepted defect severity; rejected
status means no confirmed product impact. Test changes are outside this
findings-only review.
