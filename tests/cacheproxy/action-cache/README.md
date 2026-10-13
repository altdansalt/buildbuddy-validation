# Action cache through the released proxy

Run with `bb remote test //tests/cacheproxy/action-cache/... --config=nobug`.
Every RPC uses the fixture's isolated app/proxy and disposable API key.

Coverage compares direct app and proxy behavior for missing entries, malformed
and absent digests, absent results, empty and nested instance names, repeated
updates from either endpoint, independent concurrent requests, and requested
output-file inlining. The invalidation case warms the proxy, replaces the result
upstream with a reference to a missing CAS blob, checks NOT_FOUND through both
endpoints, then uploads the blob and verifies recovery on repeated reads.
Commands and Actions are uploaded before valid updates as required by REAPI.

Behavioral references:
- [REAPI ActionCache](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
- [Released proxy implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go)
- [Released app implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/action_cache_server/action_cache_server.go)

The default proxy TTL is zero; these tests exercise authoritative revalidation.
They do not force disk eviction or use BuildBuddy's nonstandard TTL/skip-remote
headers. Missing outputs are introduced by overwriting an existing AC entry;
this tests invalidation without needing a cache deletion API.
