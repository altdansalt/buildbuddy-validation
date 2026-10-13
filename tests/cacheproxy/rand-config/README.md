# Seeded cache-proxy configurations

`TestRandomConfigurations` copies the sequential CAS/AC generator from model-based
PR #78 (commit `e4ebb1062ae7e49d50f4a44eb4aa90dcb902d0cc`) into this area. Six fresh
app/proxy fixtures alternate tiny (8–128 KiB) and roomy (64 MiB) local proxy caches.
Each configuration seed independently chooses inline-file thresholds, automatic
zstd thresholds, AC caching, local vs backend write validation, CDC enablement,
and FindMissing positive-cache TTL/capacity. The app keeps the harness's 1 GiB
cache, so local eviction must be transparent to successful write round-trips.

Before each model run, 24 distinct incompressible 32 KiB blobs oversubscribe the
tiny caches with eviction age zero and metadata accounting enabled. A final
pressure-blob sweep requires all bytes through the proxy again. Eviction is
asynchronous; the test applies pressure rather than assuming a specific victim
or exact eviction timing. Six configurations each run 180 model operations plus
final sweeps under a 90-second RPC budget; fixture startup is separately bounded
by the harness. Fixtures run sequentially and clean up between configurations.

The copied model uses batch and identity/zstd ByteStream writes, ByteStream and
batch reads, FindMissing, and AC replacement through independently chosen app
and proxy endpoints. Empty/nested instances, repeated digests, empty blobs and
binary boundary-size data are included. CAS is modeled by digest (released
Pebble intentionally deduplicates across instances); AC is modeled by instance
and action. Successful responses must match bytes/AC revisions. General misses
are legal eviction, while immediate post-write and pressure sweep reads must
succeed through the roomy authoritative app. Valid Action/Command/Directory
prerequisites are uploaded before AC updates.

Replay or expand on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/rand-config/... \
  --config=nobug --nocache_test_results --test_output=all \
  --test_arg=-randconfig.seed=314 --test_arg=-randconfig.iterations=180 \
  --test_arg=-randconfig.configurations=6
```

Seeds, full configuration choices, and operation steps are logged. On failure,
reduce configuration options and the logged operation prefix to prerequisite
writes plus the failing observation, reproduce twice, and retain a deterministic
bug-tagged target and an area finding. No automatic finding is filed.

Correctness/configuration references inspected at released v2.314.0:

- [REAPI](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto): digest validation, legal cache eviction, batch statuses, and equivalent AC update responses.
- [Pebble options](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pebble_cache/pebble_cache.go): size, inline storage, zstd and eviction settings.
- [ByteStream proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go): CDC and write-validation switches.
- [CAS proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go): FindMissing TTL/capacity.
- [AC proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go): optional local AC caching and backend revalidation.

The released proxy exposes no configurable synchronous/asynchronous write mode.
Both proxy write paths await the authoritative backend; Pebble data mutations
use `pebble.NoSync` internally without a flag. This suite does not invent such a
setting. TTL-based AC serving is an experiment with explicitly weaker freshness
and is excluded from this strict latest-value model. GetTree caching is not
varied because the copied workload does not generate trees. External peers,
GCS, registration credentials and routing targets are never randomized.

Validation: [passing default six-configuration run](https://app.buildbuddy.io/invocation/dd346766-2fab-456e-81f0-896675796914)
(122.4 seconds), dispatched by [remote runner invocation](https://app.buildbuddy.io/invocation/a4663996-7146-40f1-8143-20dbdcc23fa0).
No product defect was observed; there are no findings or bug-tagged targets.
