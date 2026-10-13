# Invalid BES sequences return Unknown instead of InvalidArgument

Title: Invalid BES sequences return Unknown instead of InvalidArgument
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: An authenticated malformed uploader receives an ambiguous error classification rather than an actionable input error. No lost acknowledged data, ordinary-client failure, authorization bypass, or server crash is demonstrated.
Status: unverified
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

The released [postProcessStream](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_server/build_event_server.go#L229-L246) explicitly constructs `status.UnknownErrorf` when sorted sequence numbers are nonconsecutive. Changing this classification should preserve rejection and the absence of commit ACKs. Independent maintainer review should decide whether the intentional retry behavior warrants retaining `Unknown`; therefore status remains unverified despite two deterministic reproductions.

Reproduce: `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bes:invalid_sequence_code --test_tag_filters=bug --nocache_test_results --test_output=all` with the public runner fetch remote configured per AGENTS.md.
