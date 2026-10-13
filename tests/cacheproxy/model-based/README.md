# Seeded cache proxy state machine

`//tests/cacheproxy/model-based:go_default_test` starts the pinned released app
and proxy using the harness. Three consecutive seeds each run 180 operations
and a final sweep. The operation generator chooses the app or proxy independently
for every step, and uses empty, flat and nested instance names, repeated digests,
empty blobs, binary payloads, boundary sizes, batch writes, uncompressed and zstd
ByteStream writes, ByteStream and batch reads, FindMissingBlobs, and AC updates
and gets. AC updates upload valid Command, Directory and Action protos first.

The reference state tracks CAS by digest across instances and seeds, and AC by
instance and action slot. Released Pebble intentionally deduplicates CAS across
instances; instance names remain part of AC keys. Each successful
read must agree with that state; NOT_FOUND and FindMissing misses are permitted
as eviction. Immediate reads from both endpoints after successful writes must
hit in the fresh, lightly loaded fixture, preventing vacuous success. AC checks
compare exit code and inline stdout/stderr, allowing server-added metadata.
Revisiting the finite action pool exercises replacement and backend changes
behind warmed proxy entries. Response ordering is never assumed for FindMissing.

Replay or expand a run on hosted runners:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/model-based/... \
  --config=nobug --nocache_test_results --test_output=all \
  --test_arg=-model.seed=314 --test_arg=-model.iterations=1000
```

All failures log the seed, step, and operation trace. The RPC phase has a shared
three-minute deadline, including the final sweeps; fixture startup has its own
harness readiness bounds. Longer iteration counts do not remove that deadline.
A failing trace should be reduced to its prerequisite writes and failing read,
then retained as a deterministic bug-tagged target after two reproductions and
fixture/configuration checks. The initial run rejected an incorrect per-instance CAS model. Source inspection
confirmed cross-instance CAS deduplication is intentional; the corrected model
uses digest keys. No product defect was identified.

Behavior references:

- [REAPI AC and CAS contracts](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto):
  content-addressed bytes, per-response batch status, legal eviction, and equivalent
  AC update responses; UpdateActionResult requires prior Action/Command uploads.
- [Released AC proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go):
  backend validation of cached results by default; no TTL opt-in is used here.
- [Released CAS proxy](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go):
  batch writes forward upstream and FindMissing cache keys include instance names.
- [Released filestore](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/filestore/filestore.go):
  `fileRecordSegments` hashes the instance name only for AC, while CAS is shared.

## Concurrent histories and restart replay

Round 2 adds twelve seeded rounds of four goroutines, each performing two AC
operations against independently chosen app/proxy endpoints. An exhaustive
linearizability search respects invocation/completion ordering and per-worker
ordering. Successful reads must match a legal register state; misses represent
legal eviction. AC writes use unique revision values and valid uploaded actions.
Concurrent ByteStream reads and identity/zstd writes share an immutable CAS
blob. Both endpoints must also return that blob after each round, including
four backend and four proxy restarts. Restarts occur at quiescent boundaries;
in-flight outage error classification is outside this test's contract.

The area-local lifecycle workaround discovers the Docker daemon by inspecting
the exact fixture container, restarts that container preserving volumes, refreshes published ports and clients, and waits for its server-type readiness endpoint. It supports both the
harness-owned daemon and a runner-prestarted daemon. Replace this helper with
shared lifecycle controls when those merge.

A failed AC history records start/end observations and confirms the trace twice.
Automatic deletion shrinking removes rounds, restart markers, and operations;
every accepted candidate must fail twice. Shrinking has a 40-replay budget, so
its output may need further reduction. Each replay recreates action prerequisites
and uses a fresh instance in the same fixture; process state is not fully reset.
RPC/lifecycle failures abort as infrastructure/configuration errors instead of
being shrunk as product defects. The oracle and shrinker have synthetic tests
for stale reads, overlapping operations, impossible values, and deletion.

The final failure prints a JSON replay argument. Pass it with shell quoting:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/model-based/... \
  --config=nobug --nocache_test_results --test_output=all \
  --test_arg='-model.trace=[{"Restart":"","Ops":[{"Worker":0,"Endpoint":1,"Write":true,"Value":1}]}]'
```

Seeds replay operation choices, not goroutine scheduling. Retain the recorded
history when a scheduling-sensitive failure does not reproduce. A finding and
bug-tagged deterministic reproducer still require investigation and two verified
reproductions; the automatic checker does not file findings.

Round 2 validation: [passing full area suite](https://app.buildbuddy.io/invocation/f6795a7f-ac34-4618-b36d-f8f6a4363653)
with `bb remote test //tests/cacheproxy/model-based/... --config=nobug`.
No product defect was found. Earlier restart readiness failures were caused by
stale Docker published ports in the local helper and were resolved by refreshing
endpoints and redialing clients.
