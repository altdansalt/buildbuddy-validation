# Warm proxy retains deleted CAS bytes

Title: Proxy FindMissingBlobs and ByteStream.Read disagree after backing CAS deletion
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0
- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: This violates the requested cross-surface agreement criterion. Independent immutable-cache retention is an architectural limitation; this finding does not claim that the released product advertises app-to-proxy invalidation.
Status: confirmed
Repro test target: //tests/statemachine/convergence-deletes:deleted_cas_agreement_repro
BuildBuddy invocations:
- https://app.buildbuddy.io/invocation/14bb4cc2-736c-4d10-a8b6-04f0b3fb70b8
- https://app.buildbuddy.io/invocation/1ca64732-b65e-4402-ad8b-b681a38acc5f

## Expected behavior

The run's agreement criterion requires app and proxy read surfaces to converge after an authorized app DeleteFile. The reproducer tests that criterion explicitly; it is stronger than the released independent-cache invalidation contract.

## Actual behavior

Upload valid bytes through the app, read them through the proxy to populate its independent cache, and delete through app DeleteFile. Over five seconds of 250ms polling, app ByteStream.Read returns NotFound and proxy FindMissingBlobs returns the missing digest, while proxy ByteStream.Read continues returning the original digest-valid bytes.

Each invocation repeats this in two fresh fixtures. Durations: first 5.0689869s / 5.071085019s, second 5.072231938s / 5.06835087s. No convergence was observed; permanent retention is not proven and eviction lifetime was not measured. Repeated reads may maintain residency.

The expanded normal suite also checks BatchReadBlobs, compressed/ranged ByteStream reads, proxy-local metadata, public GetFile/GetFileRange with proxy-backed URLs, UI downloads/views, ZIP manifests/extraction, and required CLI downloads. It models the known retention separately so default CI stays green; this isolated bug-tagged target intentionally fails the stricter requested agreement invariant.

## Logs excerpt

```text
proxy CAS agreement observation duration=5.072231938s read_status=OK
proxy FindMissing says absent but ByteStream still serves deleted bytes
```

## Notes

Only non-default flag is the app's `--enable_cache_delete_api=true`. Proxy read/revalidation settings are defaults. Source inspection of `enterprise/server/{content_addressable_storage_server_proxy,byte_stream_server_proxy}/` confirms local-first immutable reads and authoritative default FindMissing forwarding, with no app DeleteFile invalidation propagation. This rules out namespace, authentication, or unsuccessful deletion errors.

No shared harness edits or external deployments. No source-built components, fault injection, or upstream report. This is recorded as a confirmed observed consistency gap, not an additional advertised-contract defect.

## Round 2 reruns

Reproduced again in https://app.buildbuddy.io/invocation/3396e2b7-20b1-42da-a05c-1e9a3a34c24f and https://app.buildbuddy.io/invocation/700102bc-086b-415b-96f6-085f03ef6e5b (proxy kept serving bytes for the full 5s window, ~5.06s; default config plus the delete API flag).
