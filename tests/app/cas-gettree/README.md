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

Round 2 adds SHA384/SHA512 to seeded random DAG coverage, with randomized
shared-subtree edges and explicit digest functions. The generator bounds expanded
paths as well as unique nodes, since repeated directory messages are permitted. Independent BLAKE3 wire
vectors cover a three-directory chain through direct and cold/warm proxy reads
without adding a shared hash dependency. A single root with 30,000 directory
entries and 30,000 file entries is uploaded using ByteStream and compared in
full against the model through both endpoints. Eight concurrent readers overlap
24 reuploads of one complete immutable tree, alternating app/proxy writes; every
returned directory set must remain complete. The concurrent case starts with a
complete tree to avoid conflating it with the existing missing-child finding.

Round 2 validation on the pinned enterprise-v2.314.0 app/proxy images:
[default seed 314159](https://app.buildbuddy.io/invocation/c9e85f99-03fb-4f78-8243-7246bcd88301)
and [seed 271828](https://app.buildbuddy.io/invocation/ff6aedb1-cc71-4d2e-8a2d-3c6ceac481f5),
both with `--config=nobug --nocache_test_results`. No new correctness findings
were observed in these bounded cases.
