# Seeded CAS agreement properties

The test starts a digest-pinned enterprise-v2.314.0 app and cache proxy with the
shared fixture. All RPCs use fixture-local endpoints and disposable credentials.
It uses every app-advertised digest function and checks that the proxy advertises
it too. An unknown advertised function fails rather than silently losing coverage.

The default seed is 314159, with ten cases per digest function. Cases include
empty data, 1/63/64/1024-byte boundaries, random sizes, 1 MiB and 20 MiB blobs,
compressible and random bytes, mixed randomly ordered blob batches, duplicate
batch requests, and alternating app and proxy writers. Batch operations respect each endpoint's advertised limit; large
objects use ByteStream with random chunk boundaries. Both endpoints must agree
on full reads, random offset/limit reads, zstd suffix reads, batch reads and
FindMissingBlobs. Fresh absent digests must be missing and unreadable; empty
content is allowed to exist implicitly. Compression is capability-gated.

Replay or extend on the remote runner:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/crosscutting/rand-cas/... --config=nobug --nocache_test_results --test_output=all --test_arg=-randcas.seed=314159 --test_arg=-randcas.iterations=20
```

The RPC phase has a four-minute deadline regardless of the iteration setting.
Seeds and case descriptors are printed in test output. Property violations
should be reduced to an independent deterministic `bug` target and reproduced
on two remote invocations before adding a finding.

Protocol references:

- [REAPI at the repository's pinned revision](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto), CAS and ByteStream resource/offset/compression rules.
- [Released digest implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go), supported functions and resource-name segments.
- [Released ByteStream server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go).
- [BLAKE3 specification](https://github.com/BLAKE3-team/BLAKE3-specs/blob/master/blake3.pdf) and [official test vectors](https://github.com/BLAKE3-team/BLAKE3/blob/master/test_vectors/test_vectors.json). The small scalar oracle is checked against every published vector's first 32 hash bytes, including multi-chunk trees.

This area does not require shared harness or module changes. Concurrent clients,
resumable uploads, malformed requests and unsupported compressor status checks
are follow-up coverage; the property test concentrates on valid input round trips.

`compressed_read_limit_bug_test` is a separate deterministic two-byte reproducer
for accepting forbidden nonzero compressed read limits. It is tagged `bug` and
excluded by `--config=nobug`; see
[the finding](../../../../findings/crosscutting-rand-cas-compressed-read-limit.md).
