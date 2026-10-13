# Compressed ByteStream reads accept a nonzero read limit

Title: Compressed ByteStream reads accept a nonzero read limit
Components: cacheproxy / app
Image version/digest: enterprise-v2.314.0; proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Authenticated cache clients can receive a successful partial compressed response for an explicitly forbidden request. This breaks REAPI error handling and may confuse clients that expect rejection; no cross-tenant access or persistent corruption was observed.
Status: confirmed
Repro test target: //tests/cacheproxy/compression:compressed_read_limit_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/20566fa5-d48b-4e3f-b822-c3d67aff471a

## Expected behavior

The [REAPI compressed ByteStream read contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L313-L317) states: “Servers MUST return `INVALID_ARGUMENT` if `ReadRequest.read_limit` is non-zero.” A read of `limit-repro/compressed-blobs/zstd/<sha256>/31` with `read_limit=1` must therefore fail, regardless of storage representation.

## Actual behavior

Upload the 31-byte identity blob `compressed read limit reproducer` to the fixture-local app. Read its compressed resource with `read_limit=1` directly from the app and then through the proxy. Both streams terminate successfully and return 14 compressed bytes. The reproducer consumes the entire stream before checking status, so delayed gRPC errors are not overlooked.

The original coverage exposed the same behavior in two independent runs:
[79e95b27](https://app.buildbuddy.io/invocation/79e95b27-1c16-4858-a329-af4223870832) and
[ce0f4174](https://app.buildbuddy.io/invocation/ce0f4174-1778-46fc-aebc-51677fbcf5af), across eight combinations of size and upload format. Those runs also contained an unrelated protobuf structural-equality test error, since corrected. The minimal isolated target independently reproduces the limit violation in two uncached runs: [20566fa5](https://app.buildbuddy.io/invocation/20566fa5-d48b-4e3f-b822-c3d67aff471a) and [73aee7e1](https://app.buildbuddy.io/invocation/73aee7e1-c121-4c6b-af7b-f387206952f8). Both return OK and 14 compressed bytes from both endpoints.

The test uses raw ByteStream requests to fixture-local loopback endpoints, a disposable local API key, and the default supported zstd compressor. No helper changes the request's limit. Both default Pebble configuration and a 1 GiB auto-compression threshold exhibit the problem. Direct app behavior rules out proxy peer configuration or fixture forwarding errors.

## Logs excerpt

```text
--- FAIL: TestCompressedReadLimitRejected
endpoint 127.0.0.1:32768: status=OK compressed bytes=14
endpoint 127.0.0.1:32770: status=OK compressed bytes=14
expected: 0x3  (InvalidArgument)
actual  : 0x0  (OK)
Messages: compressed reads with nonzero limits must fail
```

## Notes

No image overrides or custom flags are used by the minimal reproducer. Full configs, container logs, and Docker inspect metadata are preserved by the harness in undeclared test outputs. The app is a control endpoint; only the proxy status is asserted, so a shared implementation defect does not hide proxy failure.

In released [ByteStream source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L106-L119), `CheckReadPreconditions` rejects negative limits but does not reject positive limits for compressed resources. The [proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go#L184-L198) uses this same check.

Run explicitly:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test \
  //tests/cacheproxy/compression:compressed_read_limit_bug_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all
```

Mitigation: clients should send `read_limit=0` for compressed resources, as required by REAPI. Findings remain in this repository; no upstream report was filed.

## Verification

Verdict: **confirmed**. Severity remains **low**: invalid requests succeed instead of receiving the required rejection, without demonstrated data corruption or access-control impact. This overlaps the app finding through shared validation; it is not evidence of a separate proxy-only root cause.

Independent review on 2026-10-12. The original bug-tagged target was run twice with `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/compression:compressed_read_limit_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all`. The empty tag filter overrides `nobug`; disabling test-result caching ensures both executions actually run. Components started inside fresh test fixtures on hosted runners, with no image overrides. Test logs identify the pinned digests listed above. Only disposable fixture-local credentials and endpoints were used.

Reviewed PR #10 at `b0edf6197342bf322cbb0fc7167979438712ac4e`, including the standalone `read_limit_test.go` and BUILD tags. The minimal target uses default configuration, not the auto-compression override in the separate coverage test. In both independent runs, the authenticated local app upload succeeds, and app and proxy reads each finish with OK and 14 compressed bytes. The proxy assertion alone fails. The raw request sets `ReadLimit: 1`, uses the harness-generated compressed resource name, and consumes responses through terminal EOF; late errors cannot explain the observed success.

The [REAPI compressed-read rule](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L313-L317) explicitly requires INVALID_ARGUMENT for any nonzero limit. The [pinned proxy implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go#L184-L198) calls the app's [shared precondition check](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L106-L119), which does not enforce that rule. The [app reader](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L195) accepts the limit, explaining the identical control-endpoint behavior. Forwarding the upstream success still exposes noncompliant behavior at the proxy endpoint; it does not make the test incorrect. The released [cache configuration docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md#L21) and [API docs](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/enterprise-api.md#L844) describe zstd support without a documented exception to this rule. Repeated results and the source path rule out flakiness and peer/configuration mistakes. This minimal test covers a proxy read of an app-resident blob, not every proxy cache-hit path.

Independent invocation links:
- https://app.buildbuddy.io/invocation/e9b874e7-aa10-4886-b266-50db36a01204
- https://app.buildbuddy.io/invocation/7577f3bc-dc6f-4df9-a603-7d418b23d6c8
