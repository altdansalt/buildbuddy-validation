# Corrupt batch zstd data reports an internal server failure

Title: BatchUpdateBlobs reports INTERNAL for malformed client zstd data
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: A cache-write client can trigger misleading server-failure responses with malformed input. This affects error classification and may cause unnecessary retries or alerts; no persistence of corrupt data or cross-client impact was observed.
Status: confirmed
Repro test target: //tests/app/compression:corrupt_batch_zstd_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/a95ea1b8-a612-4cf2-b513-0cef642a311a

## Expected behavior

Malformed compressed client data should return per-entry INVALID_ARGUMENT, while valid sibling requests succeed. The [REAPI CAS contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) describes BatchUpdateBlobs as equivalent to ByteStream uploads and specifies INVALID_ARGUMENT when the digest does not match the provided data. Its compressed-upload contract requires validating the uncompressed hash and size. A string that is not a zstd frame cannot represent the declared uncompressed blob. [gRPC status guidance](https://grpc.io/docs/guides/status-codes/) classifies invalid client arguments as INVALID_ARGUMENT and reserves INTERNAL for broken server invariants.

## Actual behavior

Uploading `not a zstd frame` with compressor ZSTD returns per-entry code 13 (INTERNAL), with `Failed to decompress zstd-compressed blob: invalid input: magic number mismatch`. The overall batch RPC succeeds. The valid identity-compressed sibling is stored and readable; FindMissingBlobs confirms the corrupt entry is absent.

Reproduced independently with uncached tests:

- https://app.buildbuddy.io/invocation/a95ea1b8-a612-4cf2-b513-0cef642a311a
- https://app.buildbuddy.io/invocation/5f41de89-c2cd-4b26-a0db-73a1f4f48a91

Both use fresh harness-started app containers and local API keys. The valid sibling and subsequent CAS/ByteStream checks rule out connection, auth and cache configuration failures. No external service receives component requests.

## Logs excerpt

```text
corrupt entry status: code:13 message:"Failed to decompress zstd-compressed blob: invalid input: magic number mismatch"
expected: 3
actual  : 13
malformed client data should not report an internal server failure
```

## Notes

Default harness app configuration (SQLite, Pebble, local Redis and self-auth); no version or flag overrides. The [released source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L544) explicitly wraps zstd decode errors in InternalErrorf; BatchUpdateBlobs forwards that status. This finding concerns classification only: malformed bytes are rejected. Full container logs/config/inspect metadata are preserved in test undeclared outputs. Findings remain in this repository; no upstream report was filed.

## Verification

Verdict: **confirmed**, as a client-error classification defect. Severity remains **low**: the invalid entry is rejected and absent from CAS, while the valid sibling succeeds; the observed defect is the status code, not acceptance of corrupt data. Unnecessary retries/alerts are possible client effects, not measured effects of this test.

Independent review on 2026-10-12. The original bug-tagged target was run twice with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/compression:corrupt_batch_zstd_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`. The empty tag filter overrides `nobug`; disabling test-result caching ensures both executions actually run. Components started inside fresh test fixtures on hosted runners, with no image overrides. Test logs identify the pinned digests listed above. Only disposable fixture-local credentials and endpoints were used.

Reviewed PR #18 at `68e7b320a09af146e5fdfefbbc9f49fe9c019d03`, including `corrupt_test.go` and BUILD tags. Both runs return per-entry code 13 with `Failed to decompress zstd-compressed blob: invalid input: magic number mismatch`. Before the final code assertion fails, the valid sibling has code 0, reads back correctly, and FindMissingBlobs confirms the corrupt entry is absent. The overall RPC succeeds as expected for an independently failing batch entry. This rules out auth, readiness, transport, and storage failures. The fixed malformed payload and explicit source mapping explain the repeatable result.

Spec precision: [REAPI BatchUpdateBlobs](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L372-L387) specifies independent per-entry outcomes and INVALID_ARGUMENT for digest/data mismatch; the [compressed upload rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L246-L260) require checking the uncompressed digest. Those clauses do **not explicitly assign a code to a zstd frame that cannot be decoded**; this test exercises a decoder rejection before digest comparison. The stronger classification basis is [gRPC's status definitions](https://grpc.io/docs/guides/status-codes/#the-full-list-of-status-codes): INVALID_ARGUMENT means invalid client arguments regardless of system state, while INTERNAL means broken underlying-system invariants and is reserved for serious errors. A reproducibly malformed client frame is an invalid argument, not evidence of a broken server invariant. The test's expected per-entry INVALID_ARGUMENT is appropriate, but this is not the same direct MUST violation as the compressed-read findings.

The [pinned zstdDecompress helper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L544-L551) explicitly wraps decode errors with `status.InternalErrorf`; [BatchUpdateBlobs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L313-L322) copies that code into the entry response. An explicit implementation choice alone does not make the classification contract-correct. The released [cache docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md#L21) document zstd support but no intentional INTERNAL-on-malformed-input contract. The existing [corrupt-blob source test](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server_test.go#L373-L412) expects INVALID_ARGUMENT for hash/size corruption but does not test malformed zstd; it does not justify this decoder-error code. No evidence of flakiness or unsupported compressor configuration was found.

Independent invocation links:
- https://app.buildbuddy.io/invocation/2f828115-e1cb-49c0-9553-03959617fdee
- https://app.buildbuddy.io/invocation/e72faf2d-8307-47ac-a424-516c7cb7eff6
