# Executor compression properties

Run on the hosted runner:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/compression/... --config=nobug --test_output=all
```

Replay or extend the bounded generator with `--test_arg=-compression.seed=314159`
and `--test_arg=-compression.iterations=3` (1–20 actions per configuration).
Every case logs its seed; assertion messages identify the output and size. The
four-minute RPC deadline applies per configuration. Fixture setup and teardown
are additionally bounded by the harness.

The passing matrix checks executor upload compression on/off with advertised
zstd, and direct app versus proxy cache traffic. With zstd disabled it checks
small identity-only files, with both executor compression flags disabled.
Large identity-only inputs and compressed uploads to identity-only caches are
isolated in two bug-tagged targets. Inputs alternate identity and zstd
ByteStream uploads when zstd is advertised. With zstd advertised, each action copies or transforms a
mix of empty, tiny, 99/100/101-byte boundary, 4 KiB, and randomly sized >4 MiB
compressible/incompressible files. Expected output bytes and SHA256 digests are
computed independently, and outputs are read through both app and selected cache.
Output transformations prevent input CAS hits from concealing upload failures.
Empty outputs necessarily share the standard empty digest.

## Release and protocol basis

Inspected BuildBuddy **v2.314.0** source (only inspection; tests use harness-pinned
released images):

- [Compression configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/config/config.go): `cache.zstd_transcoding_enabled` controls advertisement and ByteStream acceptance.
- [Capabilities](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go): zstd appears in both supported compressor lists when enabled.
- [Cache client](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/cachetools/cachetools.go): upload compression starts at 100 bytes; `GetBlob` forces zstd above 100 bytes even for an identity resource name.
- [Input fetcher](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/cache/dirtools/dirtools.go): small files use BatchReadBlobs; files over 2 MiB use ByteStream. `cache.client.enable_download_compression` defaults true.
- [Executor routing](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/executor/executor.go): `executor.cache_target` routes cache traffic independently of scheduler traffic; defaults route all cache traffic to that target.
- [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto): compressed resource names contain the **uncompressed** digest and size, and content must match after decompression. Identity is always supported.

A transparent test-local gRPC bridge records only executor cache requests and
forwards their payloads and metadata to the fixture app or proxy. Counts of
identity/zstd ByteStream resource names and batch compressors are logged for
each configuration. Zstd-enabled cases assert compressed streaming downloads,
zstd-accepting batch reads, and output upload encoding matching the client flag. It binds a runner port and is reachable from the executor
using the runner’s numeric network address; it never contacts a shared cache. A numeric address avoids Docker DNS and
`host-gateway` assumptions with the harness’s isolated dockerd. The checks establish
content correctness and configuration compatibility, not compression ratios.

Run the deterministic large identity-only input reproduction explicitly:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/compression:identity_large_input_bug_test --test_tag_filters=bug --nocache_test_results --test_output=all
```

The single 2,097,153-byte file is one byte above the release's BatchReadBlobs
threshold. Both executor upload and download compression are explicitly false.
The normal suite excludes this target until compatibility is fixed.

The second minimal reproduction, `:identity_small_output_bug_test`, transforms
one 100-byte input into a distinct 100-byte output. With app zstd off, download
compression off, and upload compression on, it isolates capability negotiation
for output uploads from the separate large-input download failure. Run it with
`--test_tag_filters=bug --nocache_test_results` as above.

## Validation

The [passing seeded matrix](https://app.buildbuddy.io/invocation/52fb3ce9-c24b-47fb-b197-d7d4567ecaa4)
ran 18 actions across six configurations in 233 seconds with the pinned release.
Wire counts show six zstd streaming reads per enabled configuration; large
uploads switch from `write/zstd` to `write/identity` with the upload flag, and
small output batch compressors switch correspondingly. App and proxy paths
produce identical expected output bytes and digests.

Two twice-reproduced, unverified findings document the excluded cases:

- [Large input ignores disabled download compression](../../../../findings/executor-compression-identity-large-input.md).
- [Output uploader ignores cache compressor advertisement](../../../../findings/executor-compression-unadvertised-output-zstd.md).

Broader seed sweeps, concurrent actions, and independently mismatched proxy/app
compression settings remain follow-up coverage. No shared harness or dependency
changes are required; the numeric-address bridge works around fixture DNS and
default-bridge assumptions inside this directory.
