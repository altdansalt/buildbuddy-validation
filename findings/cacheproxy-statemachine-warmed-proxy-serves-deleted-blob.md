# Warmed proxy serves a deleted CAS blob that its FindMissingBlobs reports missing

Title: Warmed cache proxy serves CAS bytes which its own FindMissingBlobs reports missing after app DeleteFile
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0
- app: gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
- proxy: gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: An explicit administrative deletion on the app does not remove bytes a proxy already cached, and that proxy contradicts itself (missing vs. readable). Requires a previously warmed blob and the non-default app flag `--enable_cache_delete_api=true`. The proxy is an independent read-through cache, so serving local data may be intended; no security isolation claim is made.
Status: confirmed
Repro test target: //tests/statemachine/public-cache-delete:warmed_proxy_delete
BuildBuddy invocation: https://app.buildbuddy.io/invocation/c33c64d6-1813-4175-8c32-635445959b1e

## Expected behavior

After the app deletes a CAS blob, the proxy should not simultaneously report the digest missing and serve its bytes. Either `FindMissingBlobs` and the readers agree on presence, or deletion invalidates the proxy's local copy. Remote-delete invalidation itself is not a documented guarantee; the self-contradiction on a single proxy is the divergence.

## Actual behavior

The test uploads a blob to the app, reads it twice through the proxy (warming it), and deletes it with `api.v1.ApiService/DeleteFile`. Control: app ByteStream read returns NotFound, and FindMissingBlobs on both the app and the proxy reports the digest missing. Divergence: proxy ByteStream read returns OK with the original bytes and proxy BatchReadBlobs returns status 0 at 0s, 5s, 30s and 60s. No convergence was seen within 60s (permanence beyond that, e.g. until local eviction, was not tested). Reproduced in both runs (run_1_of_2 and run_2_of_2) of the invocation above.

## Logs excerpt

```text
after=0s proxy_bytestream=OK proxy_batch=0
after=5s proxy_bytestream=OK proxy_batch=0
after=30s proxy_bytestream=OK proxy_batch=0
after=1m0s proxy_bytestream=OK proxy_batch=0
proxy ByteStream still serves deleted blob it reports missing
```

## Notes

Non-default config: app `--enable_cache_delete_api=true` (needed for the mutation); otherwise default harness config and pinned images. Harness error ruled out: the same digest is read through the proxy before deletion, and the app control surfaces confirm the deletion.

Root cause (buildbuddy v2.314.0 source):
- `enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go:265` reads `s.local.ReadCASResource` first and only falls back to remote on a local error.
- `enterprise/server/content_addressable_storage_server_proxy/content_addressable_storage_server_proxy.go:399` `BatchReadBlobs` reads local first.
- Same file, lines 222-236: `FindMissingBlobs` forwards to the remote (app) with `CACHE_PROXY_CAS_PASSTHROUGH` when quorum is required or the find-missing cache is disabled, so it sees the deletion; the local read paths have no delete invalidation.

The bug-tagged target is excluded from the default suite and `--config=nobug`. Run with `--test_tag_filters=bug --nocache_test_results`.
