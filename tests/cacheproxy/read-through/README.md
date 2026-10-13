# Cache proxy read-through coverage

Run `bb remote test //tests/cacheproxy/read-through/... --config=nobug`.
All requests use harness-owned, digest-pinned v2.314.0 app/proxy containers.

The tests cover backend-only CAS blobs fetched over ByteStream, compressed
read-through, concurrent cold reads, cold/warm range reads, mixed local/remote/
missing batch responses, negative reads followed by backend uploads, action
result replacement at the backend, and warm/cold reads after backend shutdown.
A partial read may populate no local entry or a complete entry; it must never
publish truncated bytes under the original digest. The `proxy_skip_remote`
header verifies local contents independently of backend fallback.

Contracts and source inspected:

- [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto): per-blob batch statuses and action-cache reads/updates.
- [ByteStream API](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto): read offsets/limits and stream completion.
- [Released ByteStream proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go): local-first reads and read-through writes.
- [Released action-cache proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go): backend is authoritative unless TTL caching is explicitly enabled.
- [Released proxy utility](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/util/proxy_util/proxy_util.go): local-only diagnostic header.

The outage test works around the absence of fixture lifecycle controls by
locating the fixture Docker socket and stopping the exact app container named
by the fixture. Each command is bounded by the test context; no shared
containers are queried by name or modified. Harness cleanup retains ownership
of all resources. A public fixture-scoped Stop/Start API would remove this
socket-discovery workaround.

Round 2 additionally covers:

- A test-local gRPC relay forwards real released-app frames and fails after
  exactly one frame, twice. The proxy must propagate `Unavailable`, leave no
  partial local CAS entry, and successfully retry once the fault is removed.
- Seeded (`20261013`) incompressible pressure on a 2 MiB cache, with zero minimum
  eviction age and disabled presence memoization. `FindMissingBlobs` proves
  the referenced output was actually evicted before the AC assertion. Backend
  eviction makes AC a miss; proxy eviction leaves backend AC usable and the
  output can be fetched again. AC metadata is reinstalled after eviction so
  eviction of the AC entry itself cannot satisfy the assertion.
- Per-request `x-buildbuddy-platform.cache-proxy-action-cache-ttl=3s`: stale
  backend replacements within TTL, expiry despite repeated reads, immediate
  visibility of updates through the same proxy, and disabled/invalid TTL values.
- Opt-in CDC interception and read fast path, enabled by a fixture-local,
  digest-pinned flagd v0.12.4 sidecar. Three seeded payloads traverse a writer
  proxy and a cold reader proxy, using identity/zstd writes, repeated whole reads,
  random ranges and compressed suffixes. Positive chunk-write and remote-chunk
  read metrics prove this did not silently test only ordinary ByteStream reads.

Multiple fixtures share the app fixture's owned Docker daemon using a
fixture-resolved `DOCKER_HOST` scoped by `t.Setenv`; they remain isolated from
other tests. Docker commands address only the exact fixture container, network,
or uniquely named sidecar. Sidecar logs are preserved with app outputs.
The relay listens on the hosted runner and the proxy addresses the fixture's
bridge gateway; no component requests reach any shared deployment.

Not covered: expiry under clock skew, every experiment targeting combination,
or a process/container crash in the middle of a read (the relay injects a gRPC
failure deterministically).
