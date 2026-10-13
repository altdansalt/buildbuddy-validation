# Digest functions

Runs against fixture-local, digest-pinned enterprise-v2.314.0 app and cache proxy.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/digest-functions/... --config=nobug
```

`TestAdvertisedDigestFunctions` checks exact Capabilities forwarding for empty
and nested instances, and exercises each of SHA1, SHA256, SHA384, SHA512 and
BLAKE3:

- Backend-only batch upload followed by repeated proxy batch and ByteStream reads.
- Proxy batch upload with direct backend readback and FindMissingBlobs.
- Empty digest recognition and batch/ByteStream reads without uploading empty data.
- Two-message ByteStream upload and direct backend/proxy readback.
- Zstandard ByteStream upload with backend and proxy readback.
- Four concurrent cold readers per digest function.
- Invalid batch hashes with per-blob INVALID_ARGUMENT on backend and proxy,
  and INVALID_ARGUMENT for invalid proxy ByteStream uploads.

BLAKE3 uses fixed empty and `abc` vectors, avoiding a shared dependency change.
The SHA functions use Go's standard library. ByteStream resource names omit the
function segment for SHA functions and include `blake3/` for BLAKE3, as required
by REAPI. Fresh instance names separate upload and read-through cases.

Contract/source references:

- [REAPI at the scaffold's pinned revision](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto):
  CAS batch statuses, ByteStream naming, and CacheCapabilities.digest_functions.
- [Supported digest functions in v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go).
- [Capabilities forwarding in v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/capabilities_server_proxy/capabilities_server_proxy.go).
- [CAS forwarding in v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go).

Round 2 adds 16 MiB streamed uploads for every advertised function and a 256 MiB
BLAKE3 upload, checking committed size, full contents on app/proxy, repeated
proxy reads, and existence. Streaming comparisons avoid accumulating responses.
`TestSeededDigestAgreement` checks 12 pseudorandom blobs across all five functions
before upload, after upload via a seeded choice of endpoint, and on repeated
reads: existence, batch statuses/data, and ByteStream contents must agree.
Replay operation order with `--test_arg=-digest_property_seed=314159` (default).
The fixed corpus uses SHA256 counter-mode bytes, with sizes around block/chunk
boundaries. BLAKE3 oracles are generated independently by
`generate_vectors.py` using Python `blake3==1.0.11`; Python is only needed to
regenerate vectors, never to run tests. No shared dependency changes are needed.
Unsupported MD5/VSO/MURMUR3/SHA256TREE and out-of-range positive/negative enum values
must reject writes with INVALID_ARGUMENT or UNIMPLEMENTED; existence/read
status codes must agree between app and proxy. MD5 uses a correctly sized hash.
UNKNOWN (omitted function) is covered by the property with SHA256 inference.
