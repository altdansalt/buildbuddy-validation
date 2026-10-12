# Auth and instance passthrough

Run with `bb remote test //tests/cacheproxy/auth-passthrough/... --config=nobug`.
All peers are fixture-local released app/proxy containers. Upstream anonymous
access is disabled so upstream fallback cannot mask missing credentials.

The action-cache matrix writes one digest with different results under two
organizations and four instances, then concurrently reads it through the app
and a shared proxy connection with alternating API keys. Requests opt into a
one-hour proxy action-cache TTL to exercise local hits. A second key belonging
to the first organization must see that organization's entry. A CAS_WRITE-only
key can upload blobs but cannot change action-cache results. Missing/invalid
credentials must fail even after caches are warmed.

CAS coverage includes batch and ByteStream uploads, direct upstream readback,
nested instance resource names, case-insensitive API key metadata, and rejected
unauthenticated streaming/unary reads. Default Pebble CAS intentionally shares
content across instances and groups; AC provides the isolation oracle here.

Contracts inspected at released BuildBuddy v2.314.0:

- [Proxy forwarding headers](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/util/proxy_util/proxy_util.go)
- [Remote authenticator](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/remoteauth/remoteauth.go)
- [Proxy AC keys and TTL](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go)
- [Pebble key isolation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/filestore/filestore.go)
- [REAPI AC and ByteStream contracts](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)

The action and command are uploaded before UpdateActionResult as required by
REAPI. Rejected credentials may return PermissionDenied (the remote
authenticator's missing-key contract) or Unauthenticated; success and server
errors are never accepted.
