# App-local DeleteFile leaves warm proxy bytes

Title: App-local DeleteFile leaves warm proxy bytes
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: Only opt-in cache deletion is affected; warm replicas can still serve deleted bytes, with no access-control bypass established.
Status: confirmed
Repro test target: //tests/statemachine/bytestream-state:go_default_test --test_arg=-test.run=^TestDeleteRecreateDivergence$ --test_arg=-test.v
BuildBuddy invocations: https://app.buildbuddy.io/invocation/456f3afc-6cb5-4eac-a1e2-307652ba4406 and https://app.buildbuddy.io/invocation/c19f093e-dfd9-4258-9cd9-a0767725dbfa
Both targets passed; the later run adds explicit proxy FindMissingBlobs missing-state and proxy re-upload controls.

## Expected behavior

The consistency goal would make app and proxy converge after deletion; subsequent successful proxy uploads should make the app observe re-created blobs (this control succeeds).

## Actual behavior

After both app-origin and proxy-origin writes warm the proxy, app DeleteFile and repeated DeleteFile remove app state; proxy FindMissingBlobs also reports missing (it forwards to app), while proxy ByteStream, BatchReadBlobs, local metadata and CLI still expose the bytes. Proxy bytes remain for the entire two-second observation per deletion. A valid repeat identity proxy Write re-creates the app blob; deleting again and re-creating on app also restores agreement. The suspected duplicate-write failure was rejected. No time-only convergence observed.

## Logs excerpt

```text
writer=app delete=0 stale proxy persisted 2.027014716s
writer=cacheproxy delete=0 stale proxy persisted 2.024886091s
writer=app proxy re-create observed on every file surface after 888.088525ms
writer=cacheproxy proxy re-create observed on every file surface after 886.332619ms
```

## Notes

Requires non-default app flag `--enable_cache_delete_api=true`. Proxy has no API-v1 DeleteFile endpoint. Source DeleteFile calls only the local cache Delete; no proxy invalidation is sent. This is a confirmed consistency difference, not a confirmed violation of a cache-invalidation contract. Observations are bounded; no infinite-duration claim.

The isolated test asserts the known difference so normal CI stays green. It must
be updated if the released behavior changes. No errors are ignored and no
component calls target shared deployments. Harness artifacts include container
configs and actual image IDs. Findings stay in this owned directory per task.

Final pinned-image full-suite verification passed in 217.6 seconds, including exact HTTP absence responses, ZIP JSON fields, required CLI reads and stable-state controls. No shared changes are required.
