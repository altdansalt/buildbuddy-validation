# App DeleteFile leaves warmed proxy CAS readable

Title: App DeleteFile does not invalidate warmed cache proxy CAS
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: Explicit cache deletion does not make artifact absence consistent across a previously populated proxy. Requires non-default deletion API flag; CAS eviction is not an access revocation contract.
Status: confirmed
Repro test target: //tests/statemachine/target-projections:delete_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/989b9f73-43b1-46fd-811f-8184652881e4

## Expected behavior

Under the requested cross-surface state-machine oracle, a successfully deleted artifact is absent through app and proxy FindMissingBlobs and ByteStream.Read.

## Actual behavior

Upload via proxy, read through both components, delete via app API, then app FindMissingBlobs reports absence. Warmed proxy still reports presence for the full 10-second bounded observation window. Repeated on a separate fixture:
https://app.buildbuddy.io/invocation/8234153c-8969-49d8-87bb-807543ba93a0

## Logs excerpt

```
app deletion did not invalidate warmed proxy within 10 seconds
run 1: deletion observation window=10.000450299s
run 2: deletion observation window=10.000831908s
```

## Notes

Non-default app flag `--enable_cache_delete_api=true`; otherwise harness defaults. Test-local components and keys only. Successful app deletion and app absence rule out failed mutation/auth. Source: enterprise/server/api/api_server.go DeleteFile calls only app cache Delete; proxy independently caches CAS. No invalidation broadcast is issued. Thus this is a persistent cache-coherence limitation until local eviction, rather than measured short asynchronous convergence. Infinite permanence was not tested; the measured lower bound is 10 seconds. This is a divergence from the requested oracle, not a claim that the API documents global invalidation. Bug-tagged isolated repro is excluded from green CI.
