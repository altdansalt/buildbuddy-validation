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
