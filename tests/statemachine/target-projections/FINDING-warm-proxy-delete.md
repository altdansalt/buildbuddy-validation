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

Under the requested cross-surface state-machine oracle, a successfully deleted artifact is absent through every read surface of both components (FindMissingBlobs, ByteStream.Read, BatchReadBlobs).

## Actual behavior

Upload via proxy, read through both components, delete via app API (`DeleteFile`). The app reports absence on every surface. The warmed proxy's `FindMissingBlobs` also reports the blob missing (no divergence there), but its `ByteStream.Read` and `BatchReadBlobs` keep returning the deleted bytes with status OK for the whole bounded observation window. Observed 60 s in each of three runs (10 s in the two earlier runs); the proxy state at the end of the window was `missing=1 read=39/OK batch=39/0` while the app was `missing=1 read=0/NotFound batch=0/5`.

Permanence: no convergence ever observed; the measured lower bound is 60 s (not tested beyond that, so "persistent until proxy-local eviction" is an inference from the source, not a measurement). A control (`TestActionCacheDeleteWarmProxy`, default suite) shows the proxy does not serve stale ActionCache entries after deletion. A cold proxy (blob never read through it before the delete) converges immediately (`TestDeleteRecreateSequenceColdProxy`, default suite).

Invocations: https://app.buildbuddy.io/invocation/989b9f73-43b1-46fd-811f-8184652881e4 , https://app.buildbuddy.io/invocation/8234153c-8969-49d8-87bb-807543ba93a0 (10 s window); 60 s window: https://app.buildbuddy.io/invocation/b1368ea2-19db-48d0-a59e-bdf1a892507e , https://app.buildbuddy.io/invocation/72ff9269-79a4-48c6-a4d1-e59ad8b49fb4 , https://app.buildbuddy.io/invocation/c00623a2-5b2e-4229-b169-c502a57dcaad (with per-reader state dump).

## Logs excerpt

```
deletion observation window=1m0.001196646s; app=[missing=1 read=0/NotFound batch=0/5] proxy=[missing=1 read=39/OK batch=39/0]
```

## Notes

Depends on non-default app flag `--enable_cache_delete_api=true` (needed to delete at all); otherwise harness defaults. Test-local components and keys only. Successful app deletion and app absence rule out failed mutation/auth. Source: enterprise/server/api/api_server.go DeleteFile calls only the app's cache Delete; the proxy keeps its own local CAS copy and no invalidation is sent. This is a divergence from the requested oracle, not a claim that the API documents global invalidation. The bug-tagged isolated repro (`delete_repro`) is excluded from green CI.
