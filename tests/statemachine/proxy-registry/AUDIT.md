# Round 2 source and assertion audit

No prior auditor branch was supplied for this item; no perturbed expectations
were imported. Inspected the v2.314.0 source checkout, without building it.

| Source | State exposure / conclusion |
| --- | --- |
| `proto/cache_proxy.proto`, `proto/buildbuddy_service.proto` | One mutator: bidirectional RegisterAndStreamHeartbeat. Readers: GetCacheProxies, ListCacheProxies, GetCacheProxy. No separate update/delete RPC. |
| `enterprise/server/cache_proxy_registry_server/cache_proxy_registry_server.go` | Summary/statistics stored on every message, including correlated detail replies. Goodbye removes state; EOF untracks live detail stream but retains list entry for ten minutes. |
| `enterprise/server/cache_proxy_registration/cache_proxy_registration.go` | Real proxy supplies summary, flags and cumulative statistics; detail inclusion flags are forwarded over the registration stream. |
| `server/buildbuddy_server/buildbuddy_server.go`, `server/libmain/libmain.go` | Registry lives on app, not cache proxy. There is no proxy-local registry reader or mutation endpoint. |
| `enterprise/app/cache_proxies/cache_proxies.tsx`, `cache_proxy_card.tsx` | UI consumes GetCacheProxies; flags and statistics are rendered from that response. No registry downloads, raw-log/JSON views or separate metadata endpoints. |
| `app/service/rpc_service.ts`, `length_prefixed.ts`, `server/http/protolet/protolet.go` | UI wire transport is application/proto, or application/proto+prefixed when streaming HTTP is enabled. JSON-only tests skipped both. |
| `cli/` searches for registry method names and cache proxy references | No bb registry read/update/delete command. Sidecar cache traffic is a different state machine. |

Round 1 assertions covered both list APIs and details over gRPC and JSON,
including genuine proxy registration and cumulative traffic counters. Gaps:
no binary UI wire checks, no same-org second key, no re-registration, only
one changing update, no bounded stability observations, and no detail inclusion
flag combinations. Legacy live-statistics comparison could pass if its entry
was missing. These are now covered; the legacy check requires entry presence.

After each synthetic heartbeat the model is compared through both list RPCs
using two same-org API keys, JSON HTTP, binary HTTP, and framed HTTP. Each
update answers GetCacheProxy through those transports for all four detail
inclusion combinations, checks forwarded flags and correlation IDs, then
rechecks persisted list state after the correlated mutations. The sequence is
create, repeat, update, update, goodbye, re-register using the second key,
repeat, update, update, goodbye. A separate EOF sequence checks retained list
state, disconnected details and eventual ten-minute expiry. Polls report first
convergence and require three consecutive successful observations 200ms apart.

Registration is explicitly enabled using fixture-local app target and disposable
key; it is optional configuration, not enabled by the baseline harness. Source
semantics do not promise registry reads on the proxy, or app-initiated registry
writes/deletes, so reversed endpoint operations do not apply to this item.
EOF retention is the documented TTL contract, not a product divergence.

## Validation

- Final suite passed (615.5s): https://app.buildbuddy.io/invocation/e30aae00-038a-4a6e-95da-1136d1599aa4
- Intermediate sequence expansion passed (617.8s): https://app.buildbuddy.io/invocation/d2849e61-6ee4-43f6-aa51-2960be379c5c
- Decoder development run rejected successful canonical-case HTTP trailers;
  this was a test bug, fixed by case-insensitive status parsing:
  https://app.buildbuddy.io/invocation/58f95439-3d78-4117-b242-e6a8f273ca94

No confirmed product divergences. EOF expiry was observed after at least 9m55s
and within the 11-minute polling bound, followed by three stable observations.
Individual convergence timings are logged with t.Logf; add
`--test_arg=-test.v` to display successful-test timings (the validation command's
`--test_output=all` alone does not enable Go verbose logging). No precise
per-transition latency is claimed from the nonverbose successful log.
