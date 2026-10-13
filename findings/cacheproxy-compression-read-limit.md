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

## Version history

Verified on 2026-10-13 using the unchanged bug-tagged target `//tests/cacheproxy/compression:compressed_read_limit_bug_test`
from `origin/area/cacheproxy-compression` at `b0edf6197342bf322cbb0fc7167979438712ac4e`.
Tests ran inside disposable fixture-local released containers on hosted runners.

| Release | Published | Result | Bazel invocation | Runner invocation |
| --- | --- | --- | --- | --- |
| enterprise-v2.314.0 | 2026-10-08 | Reproduces | [results](https://app.buildbuddy.io/invocation/ab214fa0-ff83-47c9-9644-59a8b3038d04) | [runner](https://app.buildbuddy.io/invocation/eb049c2d-145f-41cc-81ab-b2990e8c3c2a) |
| enterprise-v2.313.0 | 2026-10-06 | Reproduces | [results](https://app.buildbuddy.io/invocation/6e73827e-18af-442b-9b6f-8c73691a69bd) | [runner](https://app.buildbuddy.io/invocation/db9c3c9c-e3c9-4f4b-8e1e-7a7ac25d9ae4) |
| enterprise-v2.290.0 | 2026-07-29 | Reproduces | [results](https://app.buildbuddy.io/invocation/aacbf37e-1f48-4340-813d-15aed8ee6455) | [runner](https://app.buildbuddy.io/invocation/256d903c-ab1e-4e21-9c3e-853734a3441d) |
| enterprise-v2.275.0 | 2026-06-04 | Reproduces | [results](https://app.buildbuddy.io/invocation/5a6b83d7-3c9e-41c4-bd5a-8ff71cd6d859) | [runner](https://app.buildbuddy.io/invocation/1b9de404-2fca-46e2-9965-535de71d8564) |
| enterprise-v2.260.0 | 2026-04-16 | Startup blocked (proxy port unavailable) | [results](https://app.buildbuddy.io/invocation/4d91574b-8fda-4cbf-a1b8-0356237b74d0) | [runner](https://app.buildbuddy.io/invocation/fafe222f-5034-4854-b934-fe650f7d8d23) |

Observed in every release marked Reproduces: App and proxy both returned OK and 14 compressed bytes for read_limit=1.

Earliest tested reproducing release: **enterprise-v2.275.0 (2026-06-04)**.
The defect was present by that release; its introduction release is unknown
and may be earlier. These five samples span nearly six months and do not
establish uninterrupted behavior in untested intermediate releases.

No configuration changes were made. The four reproducing releases reached the
reported protocol assertions. On v2.260.0 both proxy targets failed during startup
at harness/fixture.go:300: `no mapping for <fixture>-cacheproxy:8080`.
The app started successfully, but the proxy never exposed its HTTP port; no
proxy RPC assertion ran. The cause/configuration adjustment was not investigated
further, per the older-release startup time limit. This is **inconclusive**, not
evidence of a fix or a new product defect. The linked undeclared outputs retain
the stopped proxy logs/config/inspect metadata for follow-up.
Image overrides changed all started components together; proxy results therefore
measure same-version app/proxy pairs, not an isolated proxy introduction date.
The manifest lookup for the initially considered v2.258.0 returned HTTP 404;
v2.260.0 was used as the April sample instead.

Exact linux/amd64 manifest pins resolved from the GCR manifest API:

| Release | Component | Image reference |
| --- | --- | --- |
| enterprise-v2.314.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843` |
| enterprise-v2.314.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906` |
| enterprise-v2.313.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:210f965d3cb47dd7c1073eff3745d6acdd31ce1e2904bb08cbd25756823086d6` |
| enterprise-v2.313.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:111df7a8d4bb57993bc78f8ed18458f32a43a93e5028f906f4c6e537e10434c6` |
| enterprise-v2.290.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:f3b285107f28da495e9496dd848857ce0cf6cddbf3fac8743b259c2a7a8678b6` |
| enterprise-v2.290.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:489014eaf088f8b8e19bee3de97c98770897edabbe614b368b74b64e72858df5` |
| enterprise-v2.275.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:75ffd43cf6dc14ccf5a652b760d7e4d7390dfa7870524c57e7f354f271e96db1` |
| enterprise-v2.275.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:8ff5709daa40bcde7968b9687d42201b71b223f50e6cdf5160c00137bfe709ad` |
| enterprise-v2.260.0 | app | `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:0f6376865cc9c92d0e837aa1cb53717b59875db68a1b09b6dd479c571f6ab88e` |
| enterprise-v2.260.0 | cacheproxy | `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:f5863331f261446428fd370bbc4c55a8317ab983121a6629e1dea1de701802de` |

Runner checkout used origin/main at `40ac83dc2c3d00efd8993a68e33a2b6df445c8fc`
with the original test-area files applied from their PR branches in a temporary
checkout; this history branch contains only findings. The reproducible command
for the current release is (replace image references with the row for an older release):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/compression:compressed_read_limit_bug_test \
  --test_tag_filters=bug --nocache_test_results --test_output=all \
  --test_arg=-harness.app_image='gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843' \
  --test_arg=-harness.cacheproxy_image='gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906'
```

Each linked invocation ran all 12 in-scope reproducers together, with explicit
app, executor, and cacheproxy digest overrides; unused components were not started.
The final bug filter overrides the default nobug exclusion, and test-result
caching was disabled. Container configs, logs, and inspect metadata are retained
in the invocations' undeclared test outputs.
