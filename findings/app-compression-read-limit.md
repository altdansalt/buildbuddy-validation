# Compressed ByteStream reads accept a forbidden read limit

Title: Compressed ByteStream Read accepts nonzero read_limit instead of INVALID_ARGUMENT
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: A cache-read client receives a successful partial compressed response for a request the protocol requires rejecting. This violates the download contract and can lead to digest mismatch or interoperability failures; valid unlimited reads remain correct.
Status: confirmed
Repro test target: //tests/app/compression:compressed_read_limit_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/c303dbd7-045b-4571-9a06-0abcbc04cc40

## Expected behavior

The [REAPI compressed download contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L313) says: “Servers MUST return INVALID_ARGUMENT if ReadRequest.read_limit is non-zero.” A Read for `read-limit/compressed-blobs/zstd/<uncompressed hash>/<uncompressed size>` with read_limit=1 must therefore fail with code 3.

## Actual behavior

The app completes the Read successfully (code 0) for read_limit=1. The dedicated reproducer receives 14 wire bytes that decode successfully to `c`, the first byte of `compressed read limit reproduction`, instead of rejecting the request. Ordinary identity reads, unlimited compressed reads and compressed reads starting at uncompressed offsets succeed and return correct data. The regression test initially exposed this behavior; the final dedicated bug target isolates the invalid request and logs its compressed/decompressed response.

Two independent runs of the final dedicated reproducer observed the same expected code 3 versus actual code 0:

- https://app.buildbuddy.io/invocation/c303dbd7-045b-4571-9a06-0abcbc04cc40
- https://app.buildbuddy.io/invocation/93261566-74fe-4a5d-8d01-7b43e222cf8c

Each uses a fresh default fixture-local released app. Successful identity and unlimited compressed reads in the same test rule out auth, harness readiness and cache configuration problems.

## Logs excerpt

```text
compressed read with limit=1: status=OK wire_bytes=14 error=<nil>
decompressed response="c" decode_error=<nil>
expected: 0x3
actual  : 0x0
compressed reads MUST reject a nonzero read_limit
```

## Notes

Default harness app configuration, with no image/flag overrides. The [released ByteStream implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L145) passes the limit to the underlying identity cache reader and recompresses the resulting data rather than rejecting it. Clients can avoid this by using read_limit=0 for compressed requests. Full container logs/config/inspect metadata are preserved in undeclared test outputs. Findings remain in this repository; no upstream report was filed.

## Verification

Verdict: **confirmed**. Severity remains **low**: this is invalid-request handling, with valid full reads working and no demonstrated persistent corruption or security impact.

Independent review on 2026-10-12. The original bug-tagged target was run twice with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/compression:compressed_read_limit_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`. The empty tag filter overrides `nobug`; disabling test-result caching ensures both executions actually run. Components started inside fresh test fixtures on hosted runners, with no image overrides. Test logs identify the pinned digests listed above. Only disposable fixture-local credentials and endpoints were used.

Reviewed PR #18 at `68e7b320a09af146e5fdfefbbc9f49fe9c019d03`, including `read_limit_test.go`, `helpers_test.go`, and its BUILD tags. Both runs pass the advertised-ZSTD capability checks, identity read and unlimited compressed-read controls, then fail only the nonzero-limit assertion: status OK, 14 wire bytes, decompressed payload `c`. The receive loop consumes the entire stream through EOF, so it does not discard a delayed RPC error. The harness builds the REAPI compressed resource name and does not alter `ReadLimit`.

The [REAPI requirement](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L313-L317) is an explicit MUST, independent of cache storage format; generic ByteStream support for limited reads does not override it. In the [pinned app implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L106-L119), `CheckReadPreconditions` rejects negative limits but accepts positive ones. [ReadCASResource](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L145) passes the limit to an identity cache reader when compressed passthrough is ineligible, then compresses that partial data. This deterministically explains the result. The released [cache documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md#L21) documents compression support, and the [API documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-api.md#L844) documents compressed resource names; neither documents an exception to the REAPI limit requirement. No harness/config error or timing-dependent failure was found.

Independent invocation links:
- https://app.buildbuddy.io/invocation/2f828115-e1cb-49c0-9553-03959617fdee
- https://app.buildbuddy.io/invocation/e72faf2d-8307-47ac-a424-516c7cb7eff6
