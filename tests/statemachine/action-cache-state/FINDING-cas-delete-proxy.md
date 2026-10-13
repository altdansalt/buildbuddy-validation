# Authoritative CAS deletion leaves warmed proxy bytes readable

Title: Public DeleteFile does not invalidate a warmed cache proxy CAS entry
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: Explicit administrative deletion does not provide global removal across configured cache tiers. Requires the opt-in deletion API and a previously warmed proxy. No cross-org exposure or corruption is established; CAS contents are still correct for their digest.
Status: confirmed
Repro test target: //tests/statemachine/action-cache-state:cas_delete_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/b90c89cc-367e-43a4-afcd-bfad4a9c0e76

## Expected behavior

The run's state-machine contract requires app and proxy to eventually agree after a successful mutation. After `api.v1.ApiService/DeleteFile` deletes a CAS resource at the authoritative app, a warmed proxy should eventually report absence too. This is a cross-tier conformance expectation, not a claim that REv2 defines a distributed deletion RPC.

## Actual behavior

A valid blob is written via the proxy; both endpoints return the exact bytes. The app accepts DeleteFile. App ByteStream.Read immediately returns NOT_FOUND and app FindMissingBlobs lists that digest. Proxy ByteStream.Read continues returning the original bytes on every 100 ms probe for the full observation window.

Independent fixture runs:

- https://app.buildbuddy.io/invocation/b90c89cc-367e-43a4-afcd-bfad4a9c0e76 — stale at 10.05566049 seconds.
- https://app.buildbuddy.io/invocation/ef30a107-bea9-419d-95f5-29c9c97f3958 — stale at 10.030992728 seconds.

Fresh verification with `--runs_per_test=2`: https://app.buildbuddy.io/invocation/83a8a8e5-e9bf-4b57-aff1-52b054a0c140 — both independent fixtures failed the same assertion; run 2 retained bytes at 10.039282492 seconds.

Classification: persistent local-cache divergence, with no observed convergence within 10 seconds. Source inspection shows DeleteFile deletes only the app cache; ByteStream proxy local-hit paths serve CAS without authoritative absence revalidation or deletion notification. It can persist until local eviction; the experiment does not prove an infinite retention time.

## Logs excerpt

```text
app absent, proxy retains original bytes after 10.05566049s: "cached before authoritative deletion"
proxy must converge after authoritative CAS deletion
expected: NotFound
actual: OK
```

## Notes

Requires non-default app `--enable_cache_delete_api=true`; all other configuration is the fixture default. Default deletion is disabled and is not misclassified as a bug. No TTL override, fault injection, remote shared peer, or image override. Same local org/key/instance/digest are used throughout. Successful before-deletion readback, successful deletion, and immediate app absence rule out auth, routing, and missing prerequisites. The bug-tagged target is excluded from normal CI and fails at the convergence assertion; run explicitly with `--test_tag_filters=bug --nocache_test_results`.

Source: `enterprise/server/api/api_server.go` (`DeleteFile`) and `enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go` (local CAS read paths). No shared harness changes required. Findings are kept here per this item's directory-only restriction.
