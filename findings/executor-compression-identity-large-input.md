# Large input downloads force zstd even when compression is disabled

Title: Executor cannot download large inputs from an identity-only cache even with compression disabled
Components: executor / app
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Valid identity-only cache configurations cannot execute actions with inputs above the batch download limit. This is an availability and interoperability defect, with no observed data corruption or cross-tenant impact. It requires disabling app zstd transcoding; the default zstd-enabled configuration works.
Status: confirmed
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

Both original fresh uncached runs returned the same status and message.
Independent verification is recorded below.

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

## Verification

Independent review on 2026-10-13: **confirmed** against the digest-pinned
enterprise-v2.314.0 images listed above. Reviewed PR #123's bug tests and shared
`runFiles` helper, the harness's recursive config merge and fixture-local peer
defaults, and release source at `974929724a4c75f824ab18fbcb560e48bbf2803a`.
The repro does not use the recording bridge, an external cache, or host API keys.
Identity input uploads succeed before execution; the action skips cache lookup
and produces distinct output bytes, so cached results/deduplication cannot hide
the failing path. RPCs and action execution have bounded deadlines.

Ran both bug targets together twice from PR #123 with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/compression:identity_large_input_bug_test //tests/executor/compression:identity_small_output_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Independent run 1](https://app.buildbuddy.io/invocation/f70118d1-1313-4d59-a318-a75be28eb082)
- [Independent run 2](https://app.buildbuddy.io/invocation/9b054abd-9635-4c1c-905f-b55af8f95cbe)

Both runs reached the same deterministic product error, rather than a readiness,
authentication, networking, or timeout failure. No intermittent outcome was
observed in these two runs; this is not a claim of exhaustive flakiness testing.

The release's [cache configuration documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-cache.md)
explicitly documents `zstd_transcoding_enabled` as controlling cache compression
capabilities. Its [flag definition](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/config/config.go#L7)
and [capabilities server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go#L41)
confirm that false is a supported setting and removes zstd from both compressor
lists. I found no documented requirement to keep app transcoding enabled for
remote execution. Server-side rejection is consistent with the configuration;
the defect is in the executor's compressor selection.

The [REAPI CAS ByteStream rules and CacheCapabilities](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L2436)
require identity support and make other compressors optional and advertised.
The [download flag and resource construction](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go#L1177)
correctly select identity when disabled, but [getBlob](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/cachetools/cachetools.go#L183)
forces zstd above 100 bytes without checking capabilities or falling back.
The 2,097,153-byte file exceeds dirtools' 2 MiB batch limit and reaches that
ByteStream path. The comment assuming the server accepts both formats is false
for this documented configuration. Both independent runs returned status 12,
`download inputs: could not fetch inputs: Unsupported compressor ZSTD`.

Medium severity retained: a valid nondefault configuration blocks large-input
execution even with both client compression flags disabled; enabling server
transcoding is a workaround. There is no demonstrated integrity or isolation
impact.

Additional [uncached control run](https://app.buildbuddy.io/invocation/b9721dbf-300c-4ba9-bd42-1de253731d82)
passed the existing `go_default_test` subcases selected with
`-test.run=^TestCompressionRoundTrips/advertise=(true|false)/proxy=false/upload=false`
and `-compression.iterations=1` (seed 314159). This covers direct-app execution
with zstd-enabled large inputs and identity-only small inputs/100-byte outputs
with upload compression disabled. These controls support the fixture and
configuration diagnosis; they are not additional runs of the bug targets.
