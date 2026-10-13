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
