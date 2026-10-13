# BatchReadBlobs enforces an unadvertised 4,000,000-byte limit

Title: App and cache proxy advertise unlimited batches but reject a 4,000,001-byte batch read
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: medium
Severity justification: Correct clients relying on GetCapabilities can receive INVALID_ARGUMENT for advertised-valid batch reads and fail artifact downloads unless they implement an undocumented batching limit or fallback. Single-blob ByteStream transfers work; no data corruption or cross-group access is implicated.
Status: confirmed
Repro test target: //tests/app/capabilities:batch_limit_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/ba586617-e988-4bd3-b4d7-a64661605007

## Expected behavior

The [REAPI CacheCapabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto) defines `max_batch_total_size_bytes=0` as no limit set, apart from actual protocol message-size restrictions. BatchReadBlobs says a server's combined blob-size limit can be obtained through GetCapabilities.

A fixed application-level batch limit should be advertised. Alternatively the application check should permit batches within actual gRPC limits. A 4,000,001-byte blob plus small protobuf overhead is below even the standard 4 MiB gRPC receive limit; the fixture client permits 64 MiB responses.

## Actual behavior

Both app and proxy return `max_batch_total_size_bytes=0`. Both accept a 4,000,001-byte blob in BatchUpdateBlobs, and ByteStream reads return the exact data. Both reject the tiny BatchReadBlobs request naming that same digest with:

`INVALID_ARGUMENT: BatchReadBlobs request exceeds server limit of 4000000 bytes`

Two uncached runs of the isolated reproducer fail identically for both endpoints:

- https://app.buildbuddy.io/invocation/ba586617-e988-4bd3-b4d7-a64661605007
- https://app.buildbuddy.io/invocation/2bd4331b-8db7-42ab-b54b-b6b856adcd59

Initial discovery with a 5 MiB blob: https://app.buildbuddy.io/invocation/5255bfc8-5a46-467b-8637-eb9e9e04f8c4.

Successful batch uploads and byte-for-byte ByteStream reads rule out missing data, incorrect digest, auth and transport setup. Small-batch regression coverage passes on both endpoints. All requests target fixture-local containers; no external cache or execution backend is involved.

## Logs excerpt

```text
--- FAIL: TestAdvertisedBatchReadLimit/app
rpc error: code = InvalidArgument desc = BatchReadBlobs request exceeds server limit of 4000000 bytes
--- FAIL: TestAdvertisedBatchReadLimit/cacheproxy
rpc error: code = InvalidArgument desc = BatchReadBlobs request exceeds server limit of 4000000 bytes
```

These are the client RPC/test excerpts. Full container/config/inspect logs are preserved by the fixture in undeclared test outputs.

## Notes

Uses default app and customer cache proxy configurations, SQLite, local Pebble and local Redis. No overrides, quotas or external services.

The released [capabilities server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go) explicitly sets `MaxBatchTotalSizeBytes: 0`. The [CAS server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go) sums uncompressed requested digest sizes and rejects them above `rpcutil.GRPCMaxSizeBytes`. That [constant](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/rpcutil/rpcutil.go) is 4,000,000, whereas [server gRPC configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/grpc_server/grpc_server.go) defaults to 50,000,000 receive bytes. This is an application check, not a gRPC message-size rejection. The proxy forwards the upstream cache advertisement and reproduces the same failure.

The test uses the smaller of a positive advertised limit and 4,000,001 bytes, so correcting the advertisement to 4,000,000 also allows the repro to pass. Mitigation: use ByteStream for larger blobs or cap batches at 4,000,000 bytes.

Run with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:batch_limit_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`. Independent verification is recorded below. Findings remain in this repository; no upstream report.

## Verification

Independent review of PR #32 at `22f3bca` against BuildBuddy
`v2.314.0` (source commit `974929724a4c75f824ab18fbcb560e48bbf2803a`):
**confirmed**. The exact pinned app/proxy image digests are listed above and
were printed by both reviewer runs. Tests were run unchanged from the PR;
this verification branch copies only the findings because PR #32 is not merged.

Reviewer command (both targets, twice, with fresh test execution):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:batch_limit_bug_test //tests/app/capabilities:proxy_execution_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- Run 1: [test invocation](https://app.buildbuddy.io/invocation/f3779d64-4048-41c3-b6ee-de8f5478732c), [runner invocation](https://app.buildbuddy.io/invocation/2d28d947-5f63-46f1-a5c7-b25914ed7276).
- Run 2: [test invocation](https://app.buildbuddy.io/invocation/8ab24a09-29c2-4a52-89c2-01a0062b0414), [runner invocation](https://app.buildbuddy.io/invocation/7964102a-8885-40ec-9adb-997526fb53aa).

The [REAPI batch-read contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L392-L408)
allows a combined blob limit, but [CacheCapabilities](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L2427-L2431)
defines zero as no configured limit except protocol message limits. The
strongest alternative explanation is the source comment "Default to protocol
limit". It does not account for this result: 4,000,001 payload bytes plus small
protobuf overhead fit below 4 MiB, the fixture permits 64 MiB responses, and
[server transport configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/grpc_server/grpc_server.go#L238-L243)
sets a 50,000,000-byte receive limit and no custom send limit.
The [CAS check](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L400-L407)
rejects digest sizes before reading data, using the separate
[4,000,000-byte constant](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/rpcutil/rpcutil.go#L29),
while [capabilities](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go#L83)
report zero. A conservative application cap is reasonable, but must be advertised.

Both endpoints passed upload and byte-for-byte ByteStream controls before the
same INVALID_ARGUMENT failure in each run; this excludes missing blobs, digest,
authentication, startup and RPC routing mistakes. No timing-dependent assertion
or executor is involved. The pinned release documentation was searched for
batch limits; no documented exception overriding REAPI was found.
Medium severity is retained because valid discovery-driven artifact reads fail;
ByteStream remains a mitigation, and no corruption is demonstrated.
