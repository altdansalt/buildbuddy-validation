# Executor compresses outputs for a cache that does not advertise zstd

Title: Executor output uploader uses zstd without consulting cache capabilities
Components: executor / app
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Successful commands cannot publish outputs of 100 bytes or larger when the app disables zstd transcoding and the executor retains its default upload compression setting. This blocks remote execution for an otherwise valid identity-only cache configuration; no data corruption or cross-tenant impact was observed.
Status: confirmed
Repro test target: //tests/executor/compression:identity_small_output_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/e8c027e8-6f86-4cf2-82fa-87c53020e43e

## Expected behavior

A cache that does not advertise zstd should receive identity output uploads.
Enabling the executor's upload compression option should use compression only
when supported by the cache. Execution and output bytes should match those with
upload compression disabled.

The [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
defines `CacheCapabilities.supported_batch_update_compressors` separately from
ByteStream compressors, and describes compressed uploads using compressors
supported and advertised by the server. Identity is always supported.

## Actual behavior

The app runs with `cache.zstd_transcoding_enabled=false`. The executor runs with
`cache.client.enable_upload_compression=true` (the release default) and
`cache.client.enable_download_compression=false`. One 100-byte input is uploaded
using identity. The command transforms it into a distinct 100-byte output.
The output size is exactly the cache client's minimum compression threshold.

Execution returns status 14 (`UNAVAILABLE`) wrapping an `UNIMPLEMENTED` output
upload error. The output uploader chooses zstd despite the app's identity-only
capabilities. Inputs fit within BatchReadBlobs, excluding the separate forced
large-download defect. Disabling upload compression succeeds in the passing
identity-only boundary coverage.

First fresh uncached reproduction:
https://app.buildbuddy.io/invocation/e8c027e8-6f86-4cf2-82fa-87c53020e43e

Second fresh uncached reproduction:
https://app.buildbuddy.io/invocation/8f24fdf6-8db2-454b-bf8e-4b1786bb7ef4

Both runs returned the same output hash, status, and wrapped upload error.

Both component containers are fixture-local, with no proxy, recording bridge,
external cache, or image overrides. Independent verification is recorded below.

## Logs excerpt

```text
deterministic reproduction seed=314159 output_size=100
Error: Should be zero, but was 14
seed=314159 status=code:14 message:"upload outputs: rpc error: code = Unimplemented desc = Error uploading file: hash:\"d6cbb053abf2933889a0ccbf6ac244623a63a2e3397e991dde09266bdaa932d1\" size_bytes:100"
```

Full configs, container logs, and inspect metadata are in undeclared test outputs.

## Notes

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/compression:identity_small_output_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

Inspected v2.314.0 source (`974929724a4c75f824ab18fbcb560e48bbf2803a`):

- [`BatchCASUploader.Upload`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/cachetools/cachetools.go) selects zstd whenever the upload flag is true and the uncompressed size is at least 100 bytes, without checking capabilities.
- [`ContentAddressableStorageServer.BatchUpdateBlobs`](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go) rejects zstd when transcoding is disabled, consistently with capability advertisement.

Mitigation: disable executor upload compression or enable app zstd transcoding.
The deterministic bug-tagged test asserts successful execution and exact output
bytes and therefore fails while the compatibility defect exists.

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

The [REAPI BatchUpdateBlobsRequest.Request.compressor rule](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a527c400875c24ae8b1a160145d7e1/build/bazel/remote/execution/v2/remote_execution.proto#L1887)
explicitly requires identity or a compressor advertised in
`supported_batch_update_compressors`. This is stronger than an assumption that
clients should negotiate compression. [BatchCASUploader.Upload](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/cachetools/cachetools.go#L1097)
selects zstd at 100 bytes solely from the upload flag; the resulting batched
request violates that rule. The flag is an enablement option, not evidence of
server support. Both independent runs returned status 14 wrapping Unimplemented
for output digest `d6cbb053abf2933889a0ccbf6ac244623a63a2e3397e991dde09266bdaa932d1`
(size 100). The input remains on the batch-read path, separating this failure
from the large-input defect.

Medium severity retained: successful actions fail to publish outputs under a
valid nondefault server configuration with the default client upload setting.
Disabling upload compression or enabling app transcoding works around it; no
integrity or tenant-isolation impact is demonstrated.

Additional [uncached control run](https://app.buildbuddy.io/invocation/b9721dbf-300c-4ba9-bd42-1de253731d82)
passed the existing `go_default_test` subcases selected with
`-test.run=^TestCompressionRoundTrips/advertise=(true|false)/proxy=false/upload=false`
and `-compression.iterations=1` (seed 314159). This covers direct-app execution
with zstd-enabled large inputs and identity-only small inputs/100-byte outputs
with upload compression disabled. These controls support the fixture and
configuration diagnosis; they are not additional runs of the bug targets.
