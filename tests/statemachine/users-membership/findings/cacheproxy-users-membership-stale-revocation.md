# Cache proxy keeps serving a removed member's key for ~60 s

Title: Cache proxy accepts a user-owned key for ~59 s after the app rejects it following UpdateGroupUsers REMOVE
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; proxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: Transient (bounded by the proxy's JWT cache TTL, default 60 s) disagreement; no permanent access. Prerequisite: the removed user already holds a warm proxy JWT. Probably an accepted cache-TTL tradeoff, but it means "removal" is not immediate across components.
Status: confirmed (transient; not a permanent defect)
Repro test target: //tests/statemachine/users-membership:revocation_bug (run with --test_tag_filters=bug --nocache_test_results)
BuildBuddy invocation: https://app.buildbuddy.io/invocation/d4e0030b-824c-464b-9745-fc01f3d085c1
Second invocation: https://app.buildbuddy.io/invocation/ede2ab6d-8a23-4977-8d8c-4332233032cc
Earlier audit evidence: https://app.buildbuddy.io/invocation/690299c7-7ca0-410c-a811-5752e4b4863e

## Expected behavior

After an org admin removes a user via UpdateGroupUsers (REMOVE), every surface agrees at once: GetGroupUsers, GetUser, GetUserApiKeys, Authenticate and app cache reads all reject the user, and the cache proxy should too.

## Actual behavior

App surfaces flip within ~20 ms (app key cache disabled with --auth.api_key_group_cache_ttl=0 to isolate the proxy). The proxy keeps serving the cached blob and rejects the same key only after 59.003 s in both runs (default proxy JWT TTL, so a default-config dependence; the untagged suite uses --auth.remote.jwt_cache_ttl=2s and converges within the 25 s bound, and TestProxyMembershipRevocation asserts eventual convergence plus 3 s of stability under the default TTL). Not a harness error: the app rejects the key in the same step.

## Logs excerpt

```text
post-removal app=rpc error: code = Unauthenticated desc = Invalid API key "K***1" proxy=<nil> proxy_data="membership revocation probe"
proxy rejected removed key after 59.003000285s
```

## Notes

Measured: 59.003 s and 59.003 s. Fixture anonymous usage is the harness default (true) in this test; not re-run with it disabled.
