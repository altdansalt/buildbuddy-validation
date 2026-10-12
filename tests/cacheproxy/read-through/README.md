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

Not covered: injected mid-stream backend failures, backend eviction while an
AC references removed CAS data, or opt-in CDC/TTL experiment paths.
