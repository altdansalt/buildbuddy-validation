# Large input downloads force zstd even when compression is disabled

Title: Executor cannot download large inputs from an identity-only cache even with compression disabled
Components: executor / app
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Valid identity-only cache configurations cannot execute actions with inputs above the batch download limit. This is an availability and interoperability defect, with no observed data corruption or cross-tenant impact. It requires disabling app zstd transcoding; the default zstd-enabled configuration works.
Status: unverified
Repro test target: //tests/executor/compression:identity_large_input_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/df192c07-266b-4951-85d8-8e9f180b737c

## Expected behavior

With `cache.zstd_transcoding_enabled=false` on the app and both
`cache.client.enable_download_compression=false` and
`cache.client.enable_upload_compression=false` on the executor, the executor
should download identity data and execute successfully. The download flag's
release-source description says it controls compression of cache downloads.

The [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
always supports identity encoding; zstd support is advertised separately in
`CacheCapabilities.supported_compressors`. A cache that does not advertise zstd
is a valid cache. A file copied by the action must retain its original bytes,
regardless of this configuration.

## Actual behavior

A single 2,097,153-byte input file (one byte above the release's 2 MiB
BatchReadBlobs threshold) prevents execution. Input upload uses identity;
the executor returns ExecuteResponse status 12 (`UNIMPLEMENTED`):
`download inputs: could not fetch inputs: Unsupported compressor ZSTD`.
The shell action never runs. The bug test asserts successful execution and
byte-identical output, so it fails until the behavior is corrected.

The case was shrunk from a seeded mixed-file matrix to one deterministic
compressible input. The reproduction uses only an app and executor started by
the harness; it has no proxy, recording bridge, shared endpoint, or external
cache. Both client compression flags are explicitly disabled. The seeded matrix
also passes for the default zstd-enabled direct app. Small identity-only input
coverage separates batching from the failing large-file download path.

First minimal reproduction:
https://app.buildbuddy.io/invocation/df192c07-266b-4951-85d8-8e9f180b737c

Second minimal reproduction:
https://app.buildbuddy.io/invocation/49a05103-2eb1-4c1b-b8e9-f057a5681b81

Both fresh uncached runs returned the same status and message. Status remains
`unverified` for maintainer review, as requested for this area.

## Logs excerpt

```text
deterministic reproduction seed=314159 input_size=2097153
Error: Should be zero, but was 12
seed=314159 status=code:12 message:"download inputs: could not fetch inputs: Unsupported compressor ZSTD"
```

Full container logs, generated configs, and inspect metadata are in the test's
undeclared outputs on the invocation.

## Notes

Run explicitly, bypassing test caching:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/compression:identity_large_input_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

Source inspected at v2.314.0 (`974929724a4c75f824ab18fbcb560e48bbf2803a`):

- [`dirtools.bytestreamReadToWriter`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go) respects `enable_download_compression` when constructing the resource name.
- [`cachetools.GetBlob`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/cachetools/cachetools.go) then replaces identity with zstd whenever the digest size exceeds 100 bytes, without consulting the flag or advertised cache capabilities.
- [`ByteStreamServer.supportsCompressor`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go) rejects zstd when transcoding is disabled, consistent with the app's advertisement.

Keeping app transcoding enabled avoids this failure. No source builds or image
version overrides were used. Findings remain in this repository.
