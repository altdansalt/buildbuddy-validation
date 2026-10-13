# Cache-proxy registry conformance

Released-source inventory (v2.314.0): `proto/cache_proxy.proto`,
`enterprise/server/cache_proxy_registry_server/cache_proxy_registry_server.go`,
`enterprise/server/cache_proxy_registration/cache_proxy_registration.go`,
`server/buildbuddy_server/buildbuddy_server.go`, and
`enterprise/app/cache_proxies/cache_proxies.tsx`.

| Mutation | Read surfaces | Sequence |
| --- | --- | --- |
| RegisterAndStreamHeartbeat initial summary | GetCacheProxies and ListCacheProxies, gRPC and UI JSON protolet | register, compare reference fields |
| Periodic summary/statistics and correlated details response | both list APIs and GetCacheProxy, gRPC and UI JSON protolet; bidirectional request/response stream | repeat heartbeat, update host and cumulative statistics, answer correlated requests after every step |
| shutting_down goodbye | both list APIs and GetCacheProxy, gRPC and UI JSON protolet | goodbye, CloseSend, receive EOF, poll absence |
| CloseSend without goodbye | both list APIs and GetCacheProxy, gRPC and UI JSON protolet | EOF retains registry entry until TTL; poll expiry |

A released proxy is explicitly configured with the fixture-local app target and
a disposable REGISTER_CACHE_PROXY key. Its identity, labels, configuration and
statistics are checked through both transports. Normal ByteStream upload/read traffic
updates the real proxy counters; a graceful Docker stop checks its goodbye. Controlled valid proxy
streams provide stable model values for update and repeated-heartbeat checks.
There is no bb registry reader or proxy-local registry read API.

The source hardcodes heartbeat staleness at 10 minutes, with no TTL configuration
flag. The expiry case therefore takes just over 10 minutes; polling uses bounded
contexts and tickers. The target sets Go's test timeout to 14 minutes (the
Go default is only 10 minutes). The root `.bazelrc` also overrides long targets
to 600 seconds, so this target is manual and its explicit command overrides
that limit to 900 seconds. Use the prestarted Docker runner so both fixtures share a
single daemon and can join the app network:

```
bb remote --runner_exec_properties=init-dockerd=true --timeout=20m test //tests/statemachine/proxy-registry:go_default_test --test_timeout=900 --test_output=errors
```

The copied proto files are unchanged released wire descriptors, parsed at test
runtime because this repository does not depend on BuildBuddy Go bindings.
The second organization uses its own disposable ORG_ADMIN reader key, and
its list views remain empty after each transition. Fixture artifacts preserve image IDs and configuration. Default images:

- app enterprise-v2.314.0: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy enterprise-v2.314.0: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
