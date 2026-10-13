# Executor compresses outputs for a cache that does not advertise zstd

Title: Executor output uploader uses zstd without consulting cache capabilities
Components: executor / app
Image version/digest: enterprise-v2.314.0; executor gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: Successful commands cannot publish outputs of 100 bytes or larger when the app disables zstd transcoding and the executor retains its default upload compression setting. This blocks remote execution for an otherwise valid identity-only cache configuration; no data corruption or cross-tenant impact was observed.
Status: unverified
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
external cache, or image overrides. Status remains `unverified` for maintainer
review, as requested for this area.

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
