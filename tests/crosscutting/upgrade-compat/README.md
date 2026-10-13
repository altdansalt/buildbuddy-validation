# Upgrade compatibility

The tests use the previous non-prerelease GitHub release, **v2.313.0**, followed
by the scaffold's pinned **v2.314.0**. The previous release was identified from
https://api.github.com/repos/buildbuddy-io/buildbuddy/releases on 2026-10-12.
Its linux/amd64 GCR manifest digests are constants in `transition_test.go`.
Both current images are explicitly selected from `harness.PinnedImages` so
ambient image overrides cannot silently change the version pair.

`TestStorageUpgrade` gracefully stops the old app and mounts its anonymous
`/data` volume into a new app fixture, retaining SQLite, disk invocation blobs,
and Pebble. The original fixture keeps ownership of the volume and old logs;
LIFO cleanup removes the new container before the original volume. The new app
gets a fresh local Redis, exercising persistent data independently of scheduler
state. The original group and API key must survive. Coverage includes empty and
binary CAS blobs, a >2 MB zstd upload, an action result with referenced output
and stdout plus inline stderr, a completed invocation and its stored events,
and a post-upgrade cache write.

`TestAppExecutorVersionSkew` exercises both old executor/new app and new
executor/old app. Each uploads command and input protos, executes a bare shell
action, verifies file output, stdout and stderr, then checks a cached execution.

Protocol and released-source references:

- [REAPI CAS and ActionCache contracts](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
- [v2.314.0 action cache implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go)
- [v2.314.0 Pebble migration implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/backends/pebble_cache/pebble_cache.go)
- [v2.314.0 BEP schema](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/proto/build_event_stream.proto)
- [v2.314.0 invocation event handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/build_event_protocol/build_event_handler/build_event_handler.go)

The BEP producer encodes a minimal started/finished event pair using stable
released-schema field numbers, wrapped in the standard Google BES messages.
No BuildBuddy source is built. Component requests use only fixture-local URLs.

The area-local Docker helper finds the daemon owning the fixture, including
private daemons created by the harness. A future harness restart/volume-sharing
API could replace this helper. Tests are sequential to keep resource usage low.

Limitations: single-node SQLite/disk/Pebble only; no MySQL/Postgres, distributed
cache, multi-release upgrade chain. The version-skew matrix covers the baseline bare
runner, not container or VM isolation providers.

Validation: [green Bazel invocation](https://app.buildbuddy.io/invocation/acca31b8-3a51-4a09-9359-cf5d78fde355)
([outer remote invocation](https://app.buildbuddy.io/invocation/764c6545-276f-44f0-8571-0cee5422d9e1)),
using plain `bb remote test //tests/crosscutting/upgrade-compat/... --config=nobug`.
All upgrade and skew cases passed; no product findings were identified.
Invocation comparisons exclude the asynchronously updated DB timestamp and
normalize an absent versus empty response-enriched scorecard and sort the
set-valued capability list; stored events and
all remaining returned metadata must match.

## Round 2

`TestStorageDowngrade` runs the same SQLite/API-key, invocation, CAS and AC
continuity checks from v2.314.0 back to v2.313.0, including a write after
downgrade. This covers data written by the current release, not merely an
untouched old volume.

`TestProxyPersistenceUpgrade` warms twelve binary CAS payloads through the
v2.313.0 proxy and replaces it with v2.314.0 using the same `/data` volume.
Payload bytes, sizes and read order use replayable seed `2026101202`. The new
proxy's cache backend points to its own closed loopback port while its
authenticator still points to the fixture-local app. Every warmed blob must
remain readable; a never-warmed remote-only blob must fail. The old proxy
linux/amd64 manifest digest is recorded in `round2_test.go` (resolved from GCR
on 2026-10-12).

`TestQueuedWorkUpgrade` retains Redis, SQLite, cache, app ports and the current
executor while upgrading the app from v2.313.0 to v2.314.0. A live executor has
one custom resource slot: a gated running shell action holds it while a second
action remains QUEUED. The queued task is checked in fixture-local Redis before
replacement, then resumed by its original operation name. No Execute is
resubmitted. The test checks queued recovery independently of the running
failure below; it does not require the running action to succeed.

The `scheduler_shutdown_bug_test` target holds one running action through
v2.313.0 -> v2.314.0 app replacement and through current-release app replacement.
Both cases repeatedly return a terminal Unavailable result after shutdown.
See [the unverified finding](../../../findings/app-upgrade-compat-running-task-shutdown.md).
The target is tagged `bug` and excluded from `--config=nobug`.

These tests cover process replacement with retained Redis; they do not test
Redis loss, distributed schedulers or exactly-once execution.

The area-local replacement helper pulls the image before downtime, mounts the
saved fixture config, retains its hostname alias, gracefully stops the component
and mounts its existing data. Scheduler checks wait for executor readiness after
replacement before reading the retained operation.
Replacement logs and inspect metadata are preserved with fixture artifacts.

Round 2 validation: the full area suite passed with default Docker in
[Bazel invocation 43f7ec14](https://app.buildbuddy.io/invocation/43f7ec14-648c-4598-a225-0e209c1670ed)
([remote run](https://app.buildbuddy.io/invocation/639bf174-e3f5-4cc0-913c-fe5e68b6e2a0)).
The final fixture, including executor readiness after app replacement, passed in
[Bazel invocation c9000be7](https://app.buildbuddy.io/invocation/c9000be7-a37f-44b1-b84a-658cb9b7e55c)
([remote run](https://app.buildbuddy.io/invocation/c668c341-c211-4380-b812-3757a43b2330))
with `--runner_exec_properties=init-dockerd=true`. One later default-daemon run
hit Docker Hub's Redis pull rate limit; prestarted Docker reuses cached images.
This infrastructure failure is excluded from product findings.
