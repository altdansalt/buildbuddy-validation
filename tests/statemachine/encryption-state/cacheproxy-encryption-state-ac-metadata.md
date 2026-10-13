# Proxy ActionResult is readable while its metadata is absent

Title: Cache proxy GetMetadata cannot observe a committed ActionResult
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0
- app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
- cacheproxy: gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: This is a metadata observability inconsistency, not lost action output, an encryption bypass, or a REv2 GetActionResult violation. Clients querying the proxy's public Cache.GetMetadata endpoint cannot inspect an AC entry which the same proxy successfully writes and reads. App/UI metadata remains available.
Status: confirmed
Repro test target: //tests/statemachine/encryption-state:ac_metadata_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/ba2a247b-e542-486f-8015-45e94ebf5800
Second invocation: https://app.buildbuddy.io/invocation/e2fe2dd7-df28-4713-a698-0146879e6d36

## Expected behavior

For the cross-surface conformance contract, after successful UpdateActionResult
through the proxy, a public metadata lookup for the same AC resource should
observe the entry once both app and proxy can read its complete ActionResult.
The app metadata lookup provides the action digest's logical size and storage
metadata. This expectation is stronger than the REv2 content-read contract:
Cache.GetMetadata is a BuildBuddy-specific API, backed by endpoint-local storage.

## Actual behavior

Upload a valid Command, input Directory and Action to the local app CAS; update
its ActionResult through the proxy. Both clients/API keys in the same org read
the full result from both endpoints. App Cache.GetMetadata succeeds, but proxy
Cache.GetMetadata returns NotFound continuously for 15.0886s / 15.0647s in two
fresh fixtures. No convergence was observed.

Classification: structural/permanent for the public action-digest metadata
lookup in this implementation, rather than an authentication/key-cache delay.
The observation window is 15 seconds; permanence is supported by source inspection,
not an unbounded runtime observation. No non-default cache TTL or enabled
encryption is required. The broad sequence also asserts this known mismatch
under disabled/enabled/re-enabled encryption, while continuing to verify every
other surface. The isolated bug-tagged test demands agreement and fails until
this mismatch is fixed; ordinary CI excludes that target.

## Logs excerpt

```text
app AC metadata exists; proxy metadata after 15.088599715s:
rpc error: code = NotFound desc = key "PTdefault/.../1/ac/v5" not found
proxy AC value is readable but its metadata never becomes visible

app AC metadata exists; proxy metadata after 15.064704603s:
rpc error: code = NotFound desc = key "PTdefault/.../1/ac/v5" not found
```

## Notes

`enterprise/server/action_cache_server_proxy/action_cache_server_proxy.go`
computes a salted digest of GetActionResultRequest and stores a pointer at that
AC key to an ActionResult stored in local CAS. Both UpdateActionResult and
GetActionResult use this representation. `server/cache_server/cache_server.go`
passes the caller's original AC resource directly to local Cache.Metadata;
`enterprise/server/cmd/cache_proxy/cache_proxy.go` registers this local server.
There is no mapping from the public action digest to the proxy's internal key
on the metadata path. Cache reads succeed because their proxy implementation
understands the internal representation and validates against the app.

Tests start fixture-local app/proxy containers, provision local org-admin keys
and use real public APIs. Default auth/key TTLs are retained in the isolated
repro; encryption remains disabled. The fixture config supplies disposable
local-insecure KMS files and client identity so the shared suite can exercise
encryption, but these are unused by this minimal repro. No cloud component
requests, fault injection, source builds, or credentials from the outer runner.
The default `cache_proxy.cache_action_results=true` setting is unchanged.
Workaround: query the app's Cache.GetMetadata or UI GetCacheMetadata endpoint.

Run twice explicitly:
`bb remote test //tests/statemachine/encryption-state/... --test_tag_filters=bug --nocache_test_results --test_output=all`.
