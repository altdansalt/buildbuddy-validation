# Malformed batch zstd data is reported as an internal server failure

Title: BatchUpdateBlobs classifies malformed client zstd bytes as INTERNAL
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: An authenticated client sending malformed compressed data receives a server-failure classification instead of an input error. This can mislead client diagnostics, retry policy and server-error monitoring. No data corruption or cross-client impact was observed.
Status: unverified
Repro test target: //tests/app/cas-batch:malformed_zstd_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/96cc87f3-6cca-46f5-9156-aa56a81b56f1

## Expected behavior

A malformed zstd frame is invalid client input and should return per-item
INVALID_ARGUMENT, leaving the valid peer successful. The
[REAPI BatchUpdateBlobs contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
requires independent item errors and lists INVALID_ARGUMENT when a digest does
not match the provided data. This finding concerns the classification of a
compression decoding error; maintainers should review that contract interpretation.

## Actual behavior

The malformed item returns INTERNAL (13), with a zstd magic-number-mismatch
message. The valid peer uploads and reads successfully in the minimal repro.
The same behavior also occurred in the initial broader compressor test:
https://app.buildbuddy.io/invocation/96d5844c-4c06-47dd-a10d-fbdfa7db26c2.

The second uncached minimal reproduction returned the same status and message:
https://app.buildbuddy.io/invocation/fbe4e3da-10b5-4505-9324-c0a0e58628e1.
Both minimal reproductions reached the assertion after verifying the valid peer
through ByteStream readback.

The fixture-local authenticated app used default configuration and the pinned
image above. Valid compressed round trips pass in the same area. The decoding
failure is intentionally caused by the test payload, not an unavailable service,
authentication failure, or invalid fixture configuration. Source inspection of
[the released helper](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go)
shows `zstdDecompress` wraps decoding errors with `InternalErrorf`.

## Logs excerpt

```text
TestMalformedZstdIsInvalidArgument
expected: 3
actual  : 13
malformed client data: code:13 message:"Failed to decompress zstd-compressed blob: invalid input: magic number mismatch"
```

## Notes

Duplicate of: [app-compression-corrupt-batch-status.md](app-compression-corrupt-batch-status.md)
(confirmed on origin/main). Both tests submit the same `not a zstd frame`
payload with compressor ZSTD to BatchUpdateBlobs on the same pinned image,
observe per-entry INTERNAL with a magic-number mismatch, and verify a valid
identity sibling succeeds. Both exercise the same zstdDecompress/InternalErrorf
path; there is no distinct defect here. This area retains its existing minimal
reproducer and unverified status, without counting it as a new finding.

Run with `GIT_CONFIG_GLOBAL=/dev/null bb remote test
//tests/app/cas-batch:malformed_zstd_bug_test --test_tag_filters=bug
--nocache_test_results --test_output=all` after setting the public fetch remote.
No image overrides, external peers, host API keys, or nondefault app flags are
used. Status intentionally remains unverified as requested for round 2 despite
repeat observations; this is a low-severity status classification finding, not
failure of batch independence. Findings stay in this repository.
