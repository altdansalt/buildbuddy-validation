# Cache proxy write-through coverage

Run on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/write-through/... --config=nobug
```

The tests start the digest-pinned v2.314.0 app and cache proxy through the harness.
They cover batch, identity ByteStream and zstd ByteStream uploads, zero-byte and
multi-message payloads, immediate direct-backend visibility, proxy read-your-writes,
concurrent duplicate writes across all three paths, mixed valid/invalid batch
members, invalid streaming digests, and backend interruption followed by retry. A second fixture enables
`cache_proxy.skip_write_validation` and checks that backend checksum rejection
is returned to the client without polluting the local cache, for identity and
zstd streams.

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

Round 2 adds bounded disk exhaustion and read-only external blob storage on the
proxy. The blob directory uses an 8 MiB tmpfs; Pebble metadata and WAL remain on
normal fixture storage. The exhaustion helper copies BusyBox and its musl loader
from the fixture's pinned Redis image into the proxy, which has no shell. It
checks ENOSPC before testing batch, identity and zstd writes and exact readback
from both endpoints. This does not exercise exhaustion of the Pebble WAL volume.

Additional coverage writes 5 MiB blobs through a proxy with a 1 MiB eviction
budget, rejects oversized gRPC messages at a 1,100,000-byte receive limit, and
checks cancellation, EOF, premature finish and declared-size overruns. Interrupted
identity and zstd uploads must remain absent at both endpoints over repeated
reads, and the same digest must accept a complete retry.

A fixture-local `ghcr.io/open-feature/flagd:v0.12.4` sidecar enables
`cache_proxy.intercept_and_chunk_large_writes`. Both released components use a
64 KiB average chunk size. The test refreshes the published proxy port after
restarting it with the flagd configuration, repeats the interruption checks, and
uses backend SplitBlob to verify multiple chunks. Container logs and the flagd
configuration are retained with the fixture artifacts. No shared deployment or
experiment service is used.

Backend-specific per-blob write errors and exhaustion of the proxy's metadata
filesystem remain outside this coverage. A bounded public fixture Docker command
method would remove daemon discovery and port-refresh workarounds.

Round 2 validation passed on hosted runners:
[first green run](https://app.buildbuddy.io/invocation/a21e2a06-d303-4ea2-ad06-16128545bd24)
and [final run including storage-failure interruption checks](https://app.buildbuddy.io/invocation/78eaae1b-d031-4e40-882b-c85468c9b8b4).
Both used `--runner_exec_properties=init-dockerd=true` and
`--nocache_test_results`. Earlier runs hit Docker Hub's anonymous Redis pull
limit; a cached prestarted daemon avoided that infrastructure failure. No
product bug was observed in this round.
