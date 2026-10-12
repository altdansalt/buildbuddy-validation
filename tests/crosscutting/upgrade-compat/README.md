# Upgrade compatibility

The tests use the previous non-prerelease GitHub release, **v2.313.0**, followed
by the scaffold's pinned **v2.314.0**. The previous release was identified from
https://api.github.com/repos/buildbuddy-io/buildbuddy/releases on 2026-10-12.
Its linux/amd64 GCR manifest digests are constants in `upgrade_test.go`.
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
cache, in-flight scheduler-state migration, downgrade, proxy persistence, or
multi-release upgrade chain. The version-skew matrix covers the baseline bare
runner, not container or VM isolation providers.

Validation: [green Bazel invocation](https://app.buildbuddy.io/invocation/acca31b8-3a51-4a09-9359-cf5d78fde355)
([outer remote invocation](https://app.buildbuddy.io/invocation/764c6545-276f-44f0-8571-0cee5422d9e1)),
using plain `bb remote test //tests/crosscutting/upgrade-compat/... --config=nobug`.
All upgrade and skew cases passed; no product findings were identified.
Invocation comparisons exclude the asynchronously updated DB timestamp and
normalize an absent versus empty response-enriched scorecard; stored events and
all remaining returned metadata must match.
