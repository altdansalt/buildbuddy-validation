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

Round 2 adds `QueryWriteStatus` checks around interrupted and completed compressed
uploads, followed by a retry from offset zero. The released app deliberately
returns `Unimplemented` for status queries; this coverage records that limitation and tests the restart fallback,
without claiming resumable upload support. Unknown,
unadvertised, identity, and uppercase compressor resource names must be rejected
without persisting an upload. Three concatenated zstd frames of seeded random
bytes exercise incompressible input whose wire size exceeds its digest size.

`TestSeededRoundTripProperty` runs 24 reproducible payload/chunking trials through
all four identity/zstd write/read combinations. Inputs alternate low-entropy and
random bytes, with boundary sizes and randomized sizes up to 300 KB. Each write
mode uses a distinct digest so an already-cached blob cannot bypass the writer.
Replay another seed with `--test_arg=-compression.seed=12345`.

Round 2 validation on the digest-pinned enterprise-v2.314.0 app passed twice:
[run 1](https://app.buildbuddy.io/invocation/6b76b337-5be7-44ad-9e0a-33ffba8bb993)
and [run 2](https://app.buildbuddy.io/invocation/2a8cf6c2-d6e1-462a-a751-26c725533861).
Both ran `bb remote test //tests/app/compression/... --config=nobug` and executed
the regression target. No additional suspected correctness defect was observed.
