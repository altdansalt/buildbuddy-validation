# OCI AC state is readable but missing from proxy metadata

Title: Cache.GetMetadata cannot describe OCI AC records readable through the proxy
Components: cacheproxy / app
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: Metadata consumers see an absent resource even though content reads succeed. No corruption, data loss, or authorization bypass observed. Requires OCI fetching enabled and a legitimate trusted internal identity to read OCI AC records.
Status: confirmed
Repro test target: //tests/statemachine/oci-fetch-state:proxy_ac_metadata_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/acc9d6f4-b5b6-4a89-99aa-eb186e65ea27

## Expected behavior

Under the cross-surface conformance goal, the same ResourceName for an OCI AC
record should be observable via Cache.GetMetadata on app and proxy when
GetActionResult through both endpoints can read that record. No content-changing
request or registry refetch should be needed to expose metadata.

## Actual behavior

After one successful FetchManifest on the app, app Cache.GetMetadata succeeds,
and proxy GetActionResult returns its OCIManifestContent. Proxy Cache.GetMetadata
for that same valid native AC resource reports NotFound on every attempt for
10.002822732s in the first isolated reproduction and 10.001087934s in the second:

- https://app.buildbuddy.io/invocation/acc9d6f4-b5b6-4a89-99aa-eb186e65ea27
- https://app.buildbuddy.io/invocation/8cf32450-6628-466b-863c-d15545393d29

The same gap was initially observed in both app-first and proxy-first sequences:
https://app.buildbuddy.io/invocation/123c5e4b-9165-479d-bb7a-923fd2ced4fe.

This is a permanent structural mismatch, rather than observed delayed convergence:
released `enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go`
`getACKeyForGetActionResultRequest` hashes the entire GetActionResultRequest for
its local AC key and stores an indirect CAS pointer. The public
`server/cache_server/cache_server.go` GetMetadata looks up the caller's original
ResourceName directly in local storage. It neither translates that key nor
consults the app. The empirical observation is bounded to ten seconds; permanence
is inferred from these two lookup paths, not an unlimited wait.

## Logs excerpt

```text
GetMetadata endpoint=127.0.0.1:32768 inst=_bb_ociregistry__manifest_content_ AC=true
proxy AC metadata remained absent for 10.002822732s; app metadata and proxy GetActionResult exist
rpc error: code = NotFound desc = key "PTdefault/91f52842eca3028d131e8d8ff778026c9f7388cf48d306a7e61d95a47554e0d7/1/ac/v5" not found
Messages: same valid AC resource must be observable via proxy metadata
```

## Notes

The same wire-encoded resource succeeds against app Cache.GetMetadata, ruling out
incorrect digest, size, instance, cache type, or digest function. Successful proxy
GetActionResult rules out upstream networking, fixture readiness, and OCI AC
identity authorization errors. The repro performs no additional FetchManifest
calls during its observation window and GetMetadata does not warm the proxy.
Only NotFound is accepted during polling; other errors fail immediately.

Non-default app config: `ocifetcher.enabled=true`; both components have the same
disposable `app.client_identity.key`, with their legitimate `app` and
`cache-proxy` client names. The RPC context has a signed executor identity and a
fixture-local API key. Proxy cache settings, including
`cache_proxy.cache_action_results=true`, are defaults. No OCI HTTP mirror config
is required by the repro. No image overrides, host secrets, shared deployments,
fault injection, artificial cache writes, or upstream reports are involved.

The target is tagged `bug` and excluded from normal CI. Run explicitly with
`bb remote test //tests/statemachine/oci-fetch-state:proxy_ac_metadata_repro --test_tag_filters=bug --nocache_test_results --test_output=all`.
This is a conformance finding: the local-only metadata implementation may be
intentional, but it does not satisfy the requested cross-surface agreement.
