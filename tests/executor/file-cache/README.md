# Executor file-cache properties

`//tests/executor/file-cache:go_default_test` starts a released app and executor
with a 1 MiB executor local file cache. All component requests address that
fixture. SHA256 digests computed by the Go harness are the reference model;
actions independently hash every input before and after a hold point.

Coverage:

- A populated-cache precondition, then an action held open while independent
  actions force eviction. The test observes removal of the held input digest
  from the cache before allowing the action to hash its input again.
- Empty inputs, filesystem block boundaries, and unique files just below, at,
  and above the cache capacity (including twice the capacity).
- Four concurrent actions with overlapping input digests, duplicate digests
  under distinct paths, and alternating executable/non-executable nodes.
- Restart of the same executor container and its persistent `/data` volume,
  followed by replay of the generated workload. A warm input is observed in
  the cache both before and after restart.

The default seed is `314159`, with 12 generated actions per concurrent/restart
phase. Replay or extend with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/executor/file-cache/... \
  --config=nobug --nocache_test_results \
  --test_arg=-filecache.seed=42 --test_arg=-filecache.iterations=24
```

Iterations are bounded to 1–100; RPCs and Docker operations share a four-minute
context, and actions have 90-second execution timeouts. Every failure includes
or follows the logged seed. The random workload is generated before concurrent
execution; scheduling remains inherently nondeterministic.

Contract references:

- [REAPI Action.input_root_digest and Directory/FileNode](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto):
  input files are identified by their content digests.
- [Released v2.314.0 filecache.go](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remote_execution/filecache/filecache.go):
  fixed-size filesystem LRU, unlinking cache-owned links on eviction;
  FastLinkFile returns a miss when linking fails, allowing CAS fallback;
  startup scans reconstruct the LRU from disk.

The test requires neither KVM nor nested containers. The harness does not yet
expose Docker lifecycle operations or its private daemon socket. The area-local
adapter discovers the socket by inspecting only the fixture's unique executor
container name. A public fixture Docker command/restart API would remove this
workaround. These tests verify bytes and successful execution, not cache-hit
rates or the exact LRU ordering. No product finding is implied by a cache miss.

Validation on the digest-pinned enterprise-v2.314.0 images:

- [Default seed 314159, 12 iterations](https://app.buildbuddy.io/invocation/045426b2-0ad5-4ca4-92d3-becc473d5e5e): passed (36.6 seconds).
- [Seed 20261013, 32 iterations, uncached](https://app.buildbuddy.io/invocation/a438097c-c5fa-4316-8855-ed6d94681f12): passed (37.8 seconds).

No failing property or product finding was observed in these runs; no
bug-tagged target was added. Further seeds and clone/copy-mode coverage are
useful future extensions rather than claims established by these runs.
