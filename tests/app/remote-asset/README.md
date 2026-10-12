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
