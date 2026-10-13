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

## Round 2

The eviction/concurrent/restart workload now runs with explicit hardlink,
`executor.local_cache_always_clone=true`, and always-clone plus
`executor.enable_fastcopy_reflinking=true` configurations. On Linux in this
release, `fastcopy.Clone` calls the hardlink implementation when reflinking is
disabled. Enabling the clone flag alone therefore does **not** establish copy
isolation. With reflinking enabled, the implementation uses `FICLONE` without a
byte-copy fallback. The test probes `/data` with `cp --reflink=always` and reports
an explicit skip if that filesystem cannot support it; it does not misclassify
an unsupported filesystem as a product defect.

`TestDockerRecycledAndFreshInputs` mounts a test-owned workspace and the fixture
Docker socket into a released executor. The local app and executor use the same
fixture daemon/network; actions have networking disabled. The action image is
`mirror.gcr.io/library/alpine@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507`.
It tests hardlink and clone-flag configurations, verifies a warmed shared digest
in the file cache, observes successful asynchronous runner pooling, then
alternates fresh Docker actions with recycled ones. Container hostnames must
remain equal for recycled actions and differ for fresh actions. Each action
checks duplicate shared inputs with different executable bits and removes and
replaces one input path. Unique random inputs exceed cache capacity to drive
eviction while the recycled runner remains pooled. All subsequent shared input
hashes must still match the uploaded bytes. In-place writes to input inodes are
outside this property; removing/replacing a path must preserve cache-owned links.

The workload generator is seeded and replayable with the same flags as round 1;
Docker scheduling is not deterministic. No local component testing is required.

Round 2 validation (`--config=nobug --nocache_test_results`, pinned v2.314.0):

| Seed | Iterations | Invocation | Result |
| --- | --- | --- | --- |
| 314159 | 12 | [Default suite](https://app.buildbuddy.io/invocation/5009321e-7c76-4835-ae35-3a9682459027) | Passed, 215.7 s |
| 42 | 20 | [Additional seed](https://app.buildbuddy.io/invocation/ba0da2e6-162d-4429-8ce8-06ca2e4db21c) | Passed, 204.9 s |
| 271828 | 24 | [Additional seed](https://app.buildbuddy.io/invocation/0f103e9a-263c-4506-9609-12ecb35d3497) | Passed, 222.4 s |

The reflink subtest explicitly skipped on the hosted filesystem:
`cp: failed to clone ...: Operation not supported`. Hardlink and clone-flag
subtests, including Docker recycling, passed. Actual reflink execution remains
unvalidated and requires a runner filesystem supporting FICLONE; there is no
independently exposed byte-copy mode in the inspected release implementation.

Initial runs completed the content/reuse checks but failed during cleanup of
root-owned `metadata/host_id` in the test's mounted directory, for example
[default](https://app.buildbuddy.io/invocation/0fe884a3-d1bd-4739-970a-466603c6a086)
and [seed 42](https://app.buildbuddy.io/invocation/5096b334-295a-4a2e-8918-1550bea1be94).
The adapter now registers root-owned directory cleanup before executor startup,
so the executor stops before removal. These were test-fixture failures, not
product findings. No bug target or finding file was warranted.
