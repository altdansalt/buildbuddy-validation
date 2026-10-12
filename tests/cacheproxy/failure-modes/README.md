# Cache proxy failure modes

Tests run only digest-pinned released containers started by the fixture.

- `TestBackendFaultsAndRecovery`: pause, stop/start, and network disconnect/connect
  the backend; check warm ByteStream and batch CAS reads, bounded miss failures,
  rejected writes during outages, and successful backend-visible writes after
  recovery. Scan proxy logs for Go panics and fatal runtime errors.
- `TestProxyRestartPersistsLocalCAS`: kill the proxy with SIGKILL, restart the same
  container/volume, and read committed CAS with remote fallback disabled.
- `TestLocalEvictionFallsBackToBackend`: constrain local storage to 2 MiB, set
  minimum eviction age to zero, write more than capacity, observe a local-only
  NotFound, and verify exact bytes remain readable through the backend and proxy.

The local-only ByteStream header is an explicit released-source feature, not a
REAPI requirement. It avoids false persistence/eviction passes caused by fallback.
Cached authentication is warmed before backend faults; a proxy restart uses the
healthy backend to authenticate the first local-only request.

Contract references:

- [v2.314.0 ByteStream proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go):
  local reads, remote fallback, and remote commit before dual-write acknowledgement.
- [v2.314.0 CAS proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go):
  warm batch reads can be served locally.
- [v2.314.0 proxy utilities](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/util/proxy_util/proxy_util.go):
  `proxy_skip_remote=true` makes local storage authoritative for ByteStream.
- [v2.314.0 Pebble cache](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pebble_cache/pebble_cache.go):
  capacity eviction and configurable minimum eviction age (default six hours).
- [REAPI](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto):
  CAS bytes are addressed by digest; transient backend failure must not be
  interpreted as authoritative blob absence.

Run with `bb remote test //tests/cacheproxy/failure-modes/... --config=nobug`.
The area-local Docker helper discovers a fixture-owned daemon by inspecting the
fixture's container name. It mutates only that fixture's containers/network.
A harness Docker command/restart API would remove this workaround.
