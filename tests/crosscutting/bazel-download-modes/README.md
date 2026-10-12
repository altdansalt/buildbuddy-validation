# Bazel download modes

This suite downloads the SHA256-verified Bazel 7.4.1 standalone Linux client
inside the hosted runner. It builds two chained genrules against only the
harness-local released app/executor, using bare-runner execution. It never uses
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
  or Tree message,
  then serve the same entry after the bytes are restored. Action and Command
  protos are uploaded as required by REAPI. This constructs the post-eviction
  state; it does not exercise the Pebble eviction mechanism itself.
- Execution bypasses a dangling cached result, regenerates missing bytes, and
  serves the repaired result as a cache hit on the next request.

Contracts used:

- [Bazel download flags](https://bazel.build/reference/command-line-reference#flag--remote_download_outputs)
- [REAPI ActionCache](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
  says referenced CAS blobs SHOULD be available when returning an ActionResult.
- [v2.314.0 action cache validation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go)
  checks output files and files in output trees before returning a hit.

Run with `GIT_CONFIG_GLOBAL=/dev/null bb remote test
//tests/crosscutting/bazel-download-modes/... --config=nobug`.
