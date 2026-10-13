# Remote Asset validation

These tests address only the digest-pinned enterprise app started by `harness.Start`.
The origin is an HTTP listener owned by the test on the hosted runner. The app's
`remote_asset.allowed_private_ips` allowlist includes only that runner interface
(`/32`); this is necessary because private origins are denied by default. No
external asset origins or shared BuildBuddy deployments are contacted. A bounded
Fetch-to-CAS preflight waits for the app's internal gRPC client to reconnect after
startup; HTTP readiness alone can precede that reconnection.

Run passing coverage:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset/... --config=nobug
```

Run the defect reproducers (expected failures on the pinned release):

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/remote-asset:origin_status_bug //tests/app/remote-asset:freshness_bug --test_tag_filters=bug --nocache_test_results --test_output=all
```

Protocol references:

- [Remote Asset API at the repository's pinned remote-apis revision](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/remote_asset.proto)
- [checksum.sri qualifier lexicon](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/asset/v1/qualifiers.md)
- [Released Fetch implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_asset/fetch_server/fetch_server.go)
- [Released Push implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_asset/push_server/push_server.go)

Push and FetchDirectory explicitly return UNIMPLEMENTED in this release. The
regression target documents that capability boundary; it does not report optional
Push support as a conformance defect. Concurrent requests verify successful
content and CAS availability without requiring an optional coalescing policy.

Round 2 adds the `followups` target:

- Seeded payload (seed `731902`) checks SHA256 SRI cache reuse and conversion
  into SHA1/SHA512 CAS entries, including digest, size, digest-function response,
  ByteStream readback and absence of additional origin requests.
- Local HTTP 307 redirects preserve URL-specific headers; per-URL overrides
  take precedence over shared headers and successive requests remain independent.
- Four overlapping checksum-identical fetches hold the local origin open. The
  first caller is cancelled; the three remaining callers must finish and read
  the expected CAS content. A short overlap window lets their RPCs reach the app;
  origin hit counts are not asserted because coalescing is optional.

The existing `origin_status_bug` and `freshness_bug` sources are unchanged.
Round 2 observed no additional suspected defects on the pinned image.

Validation: [non-bug suite](https://app.buildbuddy.io/invocation/44352f89-a19a-40ec-9796-47cb5fda18a5).
