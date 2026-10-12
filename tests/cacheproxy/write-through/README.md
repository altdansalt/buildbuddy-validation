# Cache proxy write-through coverage

Run on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/write-through/... --config=nobug
```

The tests start the digest-pinned v2.314.0 app and cache proxy through the harness.
They cover batch, identity ByteStream and zstd ByteStream uploads, zero-byte and
multi-message payloads, immediate direct-backend visibility, proxy read-your-writes,
concurrent duplicate writes across all three paths, mixed valid/invalid batch
members, invalid streaming digests, and backend interruption followed by retry.

The release implements synchronous backend writes: CAS BatchUpdateBlobs returns
its remote response, and ByteStream dualWrite closes the remote stream before
returning success. Neither path has an asynchronous write mode in the tested
configuration. References:

- [Pinned CAS proxy source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go)
- [Pinned ByteStream proxy source](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go)
- [REAPI CAS and ByteStream contracts](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)

Backend interruption pauses only this fixture's app container after warming
proxy authentication. A three-second RPC deadline bounds each interrupted write;
then the test resumes the backend and verifies successful retry data directly.
A failed write may have committed before its deadline, so the test deliberately
does not assert absence of valid data after cancellation.

The harness does not expose a container-control method. The area-local helper
locates the default or private fallback Docker socket by checking for the exact
fixture container ID, then pauses/unpauses only that container. A public bounded
fixture Docker command method would remove this workaround.

This covers backend transport interruption, not injected disk exhaustion,
backend-specific per-blob write errors, feature-flagged chunking, or local-cache
write failures.
