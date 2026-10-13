# Splice publishes readable content absent from app range and metadata surfaces

Title: Manifest-only SpliceBlob state is invisible to app limited reads and metadata
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: medium
Severity justification: A successful valid SpliceBlob publishes bytes readable by unlimited ByteStream reads, but app range downloads fail and UI metadata incorrectly reports absence. This breaks clients using ranges/metadata after chunked publication.
Status: confirmed
Repro test target: //tests/statemachine/convergence-structures:splice_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/e84b02eb-7517-4d79-9ba3-9c1cd8959b49

## Expected behavior

After SpliceBlob acknowledges a valid SHA256 blob, ByteStream.Read with a valid offset/limit and ApiService.GetFileRange should expose the corresponding bytes, and GetCacheMetadata should expose its existence and digest size. App and proxy should agree.

## Actual behavior

Upload two 1 MiB chunks, then splice their concatenation into a previously absent 2 MiB digest. Full unlimited reads on app and proxy return the exact concatenated bytes. App limited ByteStream.Read, ApiService.GetFileRange and BuildBuddyService.GetCacheMetadata repeatedly return NotFound. Proxy limited reads succeed after its full read warms the local cache.

Independently reproduced twice:
- https://app.buildbuddy.io/invocation/e84b02eb-7517-4d79-9ba3-9c1cd8959b49 — unchanged failure after 10.010440924 seconds.
- https://app.buildbuddy.io/invocation/703f7619-81db-4ba0-a2b9-4adc3a21633c — unchanged failure after 10.177056165 seconds.

Persistence: no convergence during either observation window. Source shows a permanent representation gap until a flat copy of the full digest is separately uploaded; waiting alone cannot materialize it. The test does not claim an experimentally measured infinite duration.

## Logs excerpt

```text
after 10.177056165s ByteStream limited read side=0:
rpc error: code = NotFound desc = key "PTdefault/5ef6e6cdabf83b1148e095353ceea725d1f790ef8559def9e23aeb43f192e0d2/1/cas/v5" not found
GetFileRange: rpc error: code = NotFound desc = key "PTdefault/5ef6e6cdabf83b1148e095353ceea725d1f790ef8559def9e23aeb43f192e0d2/1/cas/v5" not found
GetCacheMetadata: rpc error: code = NotFound desc = key "PTdefault/5ef6e6cdabf83b1148e095353ceea725d1f790ef8559def9e23aeb43f192e0d2/1/cas/v5" not found
```

## Notes

Repro explicitly sets app `--cache.min_chunked_read_fallback_size_bytes=1048576` and proxy `--cache_proxy.disable_cdc=false` to expose chunking on small deterministic input; no faults or invalid inputs. The reference digest/chunk order and auth are validated by successful complete reads. Source: `server/remote_cache/chunking/chunking.go` ShouldReadChunked requires `limit == 0`; ApiService.GetFileRange calls Cache.Reader directly; `server/cache_server/cache_server.go` GetMetadata calls Cache.Metadata directly. These paths have no manifest fallback. UNKNOWN chunking function permits the supplied boundaries.

The main green suite explicitly checks these known NotFound boundaries for manifest-only app blobs, while still asserting every successful byte surface against the model. The bug-tagged isolated test asserts the desired behavior and fails until fixed. Run `bb remote test //tests/statemachine/convergence-structures:splice_repro --test_tag_filters=bug --nocache_test_results --test_output=errors`.
