# Bazel download modes

This suite downloads the SHA256-verified Bazel 7.6.1, 8.4.2 and 8.5.0 standalone Linux clients
inside the hosted runner. It builds two chained genrules against only the
harness-local released app/executor with the cache proxy in the cache path, using bare-runner execution. It never uses
the dispatch API key for component requests.

Coverage:

- `minimal`: neither intermediate nor final output is materialized.
- `toplevel`: final output is materialized, intermediate remains remote.
- `all`: both outputs are materialized.
- Every mode uses `--remote_cache_async`, with and without `--disk_cache`.
  Additional local-execution cases force fresh async uploads rather than merely
  consuming executor-uploaded outputs. Every case has a separate remote instance.
- A second fresh output base verifies cache reuse and the same materialization
  policy; batch process exit provides the asynchronous upload completion barrier.
- App and proxy reject AC entries with a missing output file, nested tree leaf,
  or Tree message, then serve the same entry after the bytes are restored. Action and Command
  protos are uploaded as required by REAPI. This constructs the post-eviction
  state; it does not exercise the Pebble eviction mechanism itself.
- Execution bypasses a dangling cached result, regenerates missing bytes, and
  serves the repaired result as a cache hit on the next request.
- An actual eviction test builds with minimal downloads, fills a private 8 MiB
  Pebble cache with incompressible blobs, proves the output bytes return
  `NOT_FOUND`, and builds a new local consumer in the same Bazel output base. Both app and proxy
  caches are churned, and both must return `NOT_FOUND` before the consumer runs.
  Both caches have zero minimum eviction age and a one-second sampler refresh;
  these test-only settings make eviction practical without touching other caches.

Contracts used:

- [Bazel download flags](https://bazel.build/reference/command-line-reference#flag--remote_download_outputs)
- [REAPI ActionCache](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
  says referenced CAS blobs SHOULD be available when returning an ActionResult.
- [v2.314.0 action cache validation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go)
  checks output files and files in output trees before returning a hit.

Run with `GIT_CONFIG_GLOBAL=/dev/null bb remote test
//tests/crosscutting/bazel-download-modes/... --config=nobug`.

The root `bazel-*` ignore pattern matches this directory name. New files here
need `git add -f` until the coordinator narrows that pattern to root outputs.

Round 2 adds three simultaneous async-upload clients sharing 12 seeded outputs
(seed `314159`), followed by a fresh read-only cache client that must hit every
action and verify every byte. Eviction churn uses replayable seed `20261012`.
Bazel 7 uses its experimental async flag; Bazel 8 uses the stable flag.

Validation: [initial matrix run](https://app.buildbuddy.io/invocation/44df54ac-8dc3-4dda-82bd-26db87356120)
and [pushed branch run with forced concurrent uploads](https://app.buildbuddy.io/invocation/e3477ca0-ba3e-4cb8-a20e-fe664396c99d)
passed on the default digest-pinned enterprise-v2.314.0 images. No defect was observed.
