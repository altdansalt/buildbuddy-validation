# Cache proxy compression

Tests use only the fixture's digest-pinned enterprise-v2.314.0 app and proxy.
Both Pebble caches set `cache.pebble.min_bytes_auto_zstd_compression` to 1 GiB,
so identity uploads remain uncompressed while explicitly compressed uploads
exercise zstd storage. No executor or external cache is used.

Coverage includes cold/warm cross-format reads, decompressed read offsets,
nonzero compressed read limits, split-frame upload offsets and committed sizes,
incompressible concurrent identity/zstd uploads, already-complete uploads,
malformed frames, digest/size mismatches, batch compressor negotiation, and
upstream readback after proxy writes.

Contracts are based on the [REAPI ByteStream conventions](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L219-L326)
and the released source:

- [ByteStream proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go)
- [CAS proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go)
- [Pebble representation conversion](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pebble_cache/pebble_cache.go)

Run with `bb remote test //tests/cacheproxy/compression/... --config=nobug`.
Malformed zstd frames must fail and remain absent; their exact status is not
asserted because the REAPI digest-mismatch rule does not specify decoding-error
status. Valid zstd with a mismatched hash or uncompressed size must return
`INVALID_ARGUMENT`.
