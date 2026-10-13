# Digest-function execution

Runs only fixture-local released app, executor, cache proxy and Redis containers.
Uploads go through the proxy; Execute goes to the app; output CAS/ByteStream reads
and AC reads go through the proxy, with AC also checked directly on the app.

The property test generates binary and repetitive payloads from a logged seed.
The default six cases include empty inputs, SHA block boundaries and BLAKE3 chunk
boundaries, plus a multi-chunk input. Each identical case executes with SHA256,
SHA1, SHA384, SHA512 and BLAKE3. Checks cover output bytes, output-file digests,
Tree digests, nested DirectoryNode/FileNode digests, optional root directory
digests, stdout/stderr digests, action-cache retrieval and cached execution.
Querying each action under every other function must fail with NOT_FOUND or
INVALID_ARGUMENT. Mixed-function commands and input files must fail with
INVALID_ARGUMENT or FAILED_PRECONDITION, without crashing any component.
The fixture saves logs; cleanup checks them for Go panic/fatal-error markers.

The area-local BLAKE3 reference follows the official reference implementation
and is checked against official empty/abc and block/chunk/tree-boundary vectors.
No new shared dependency is required.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/digest-function-execution/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/digest-function-execution/... --config=nobug --test_arg=-digest.seed=12345 --test_arg=-digest.iterations=30 --nocache_test_results
```

Iterations are restricted to 1..100; the property RPC phase is bounded to four
minutes, with 20-second action timeouts. The seed is printed before fixture
startup and included in every property subtest name.

Contract and pinned release references:

- [REAPI execution digest invariants](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
  (`ExecutionCapabilities.digest_functions`, `ExecuteRequest.digest_function`,
  and ByteStream resource naming).
- [v2.314.0 advertised functions](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go).
- [v2.314.0 hashing and resource naming](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/digest/digest.go).
- [Official BLAKE3 vectors](https://github.com/BLAKE3-team/BLAKE3/blob/master/test_vectors/test_vectors.json).

## Round 2

`TestConcurrentDigestFunctionsAndSymlinks` runs seeded waves of five parallel
executions (one per advertised function), with both the default app cache target
and `executor.cache_target=grpc://cacheproxy:1985` plus
`executor.cache_target_traffic_percent=100`. Scheduling still goes to the local
app. The first wave has empty content; later waves have seeded binary payloads
up to 8192 bytes. All functions use the same command and payload in each wave.
Actions are uploaded before the wave begins; parallel subtests issue Execute
concurrently, and the command briefly sleeps to increase overlap. This checks
correctness under concurrent requests without assuming the scheduler runs all
five actions simultaneously.

Each result is checked directly through app and proxy CAS/ByteStream and AC,
and via cached Execute. Checks include nested directory digests, nested relative
and dangling symlinks, top-level file/directory/dangling symlink paths and targets,
and rejection of AC lookups under every other function. The result check asserts
exact symlink counts, so duplicate paths cannot pass the map comparison.

The release's separate cache route and percentage options are declared in
[executor.go at v2.314.0](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/cmd/executor/executor.go).
No shared harness or dependency changes are needed. Replay with the same
`-digest.seed` and `-digest.iterations` flags documented above; select just round 2
with `--test_filter=TestConcurrentDigestFunctionsAndSymlinks`.

The harness starts the executor before the proxy. The proxy-route case therefore
performs a bounded SHA256 warm-up before its measured waves, retrying only
UNAVAILABLE responses containing the initial `lookup cacheproxy ... no such host`
DNS failure. This accommodates gRPC's reconnect timer after proxy startup; other
errors fail immediately, and measured waves have no retries. REAPI permits
populating deprecated symlink result fields for v2.0 compatibility; if present,
these must agree with the modern output symlink paths and targets.
