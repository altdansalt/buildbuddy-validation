# App compression conformance

Tests use only the released app started by the harness. The oracle is the
[REAPI contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
and [BuildBuddy v2.314.0 source](https://github.com/buildbuddy-io/buildbuddy/tree/v2.314.0/server/remote_cache).
No server is built from source.

`regression_test` checks advertised zstd capabilities, empty and nonempty blobs,
nested instance/resource names, ByteStream frame splitting and wire-byte offsets,
committed size, batch and stream uploads, compressed/identity downloads, acceptable
batch compressors, uncompressed read offsets, digest/size validation, concurrent
mixed uploads, and rejection/non-persistence of corrupt and truncated streams.

Bug targets isolate corrupt batch error classification and forbidden nonzero
compressed read limits. Findings are in `findings/app-compression-*.md`.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/compression/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/compression/... --test_tag_filters=bug --nocache_test_results
```
