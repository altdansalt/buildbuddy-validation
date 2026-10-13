# With anonymous usage enabled, an unauthenticated caller reads a blob written under an org key

Title: auth.enable_anonymous_usage=true lets unauthenticated gRPC/HTTP callers read a blob that an org API key wrote (membership removal is moot)
Components: app
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium (config-dependent)
Severity justification: Requires the non-default setting auth.enable_anonymous_usage=true (the validation harness default). Then any unauthenticated caller can read org data by digest, and an invalid/removed key falls back to anonymous in HTTP. Possibly intended anonymous-cache semantics, so severity is capped pending maintainer intent.
Status: confirmed (behaviour reproduced 5 times; intent unverified)
Repro test target: //tests/statemachine/users-membership:download_bug (--test_tag_filters=bug --nocache_test_results)
BuildBuddy invocation: https://app.buildbuddy.io/invocation/d4e0030b-824c-464b-9745-fc01f3d085c1
Second invocation: https://app.buildbuddy.io/invocation/ede2ab6d-8a23-4977-8d8c-4332233032cc

## Expected behavior

A blob uploaded with an org key is only readable with credentials that currently belong to that org. After UpdateGroupUsers REMOVE, /file/download with the removed member's key must fail, matching the gRPC cache read of the same key (Unauthenticated).

## Actual behavior

With anonymous usage on: the removed key, a bogus key and no credentials all get 200 and the blob from /file/download, for the full 45 s observation window, and a no-credentials gRPC ByteStream read of the same digest also succeeds. With auth.enable_anonymous_usage=false (TestRemovedMemberHTTPDownloadNoAnonymous, untagged, passes) the same sequence is denied; the main suite also passes with it disabled. So this is not membership state propagation: it depends on the anonymous-usage setting.

## Logs excerpt

```text
control: unauthenticated gRPC read of the same blob: <nil>
removed key: HTTP 200 "membership http download probe" (gRPC removed-key read: Unauthenticated Invalid API key)
bogus key: HTTP 200 ...    no credentials: HTTP 200 ...
```

## Notes

Permanent while the setting is on (no convergence observed in 45 s). The blob was written with an org admin key (not anonymously), so the anonymous read crossing the org boundary is the surprising part; maintainers should confirm whether anonymous mode intentionally shares one namespace.
