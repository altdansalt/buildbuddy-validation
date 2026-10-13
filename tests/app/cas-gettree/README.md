# CAS GetTree conformance

The passing target generates deterministic DAGs from `-gettree.seed` (default
314159) and `-gettree.iterations` (default 24 per digest function). Trees include
a depth-100 chain, a 128-edge wide root, repeated subtree digests, files, and
SHA256/SHA1 digests, with explicit and inferred digest functions and default/large
page sizes. A reference graph walk computes the reachable directory
set independently of the returned stream; duplicate messages are permitted,
while unexpected or absent content fails the invariant. Every randomized
subtest name includes the seed, digest function, and iteration. Reads repeat
through the app and the cache proxy to cover warm caches. Each RPC sequence has
a bounded context; larger iteration runs may require increasing the test's
three-minute RPC budget in this directory.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree/... --config=nobug
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:go_default_test --test_arg=-gettree.seed=42 --test_arg=-gettree.iterations=48 --nocache_test_results
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/cas-gettree:bugs_test --test_tag_filters=bug --nocache_test_results
```

Fixed bug cases cover empty roots/children, a two-directory tree requested with
page_size=1, and missing roots/children. The page-size test also compares issued
token resumes with the original stream's suffix. The pinned server issues no
tokens for this case, so token stability cannot currently be exercised.
Arbitrary fabricated page tokens are excluded because the REAPI contract
requires a token issued by the server. Fresh requests compare content sets,
not traversal order, which the spec explicitly leaves unspecified.

Contract: [REAPI GetTree and message definitions](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).
Implementation inspected: [v2.314.0 CAS server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go).
All component requests use test-local harness containers and disposable keys.
