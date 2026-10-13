# FindMissingBlobs reports a successfully spliced blob missing

Title: Manifest presence lookup uses the automatic-upload size threshold
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: False CAS absence causes redundant uploads despite a verified accessible blob. Requires explicit valid chunk publication below the automatic-upload threshold, not ordinary automatic chunking.
Status: confirmed
Repro test target: //tests/statemachine/cas-chunk-state:chunk_only_presence_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/4e7d7c45-c0a4-4f29-8767-5cd7a4474a1e

## Expected behavior

After a successful content-verified SpliceBlob, FindMissingBlobs must report the published digest present. The request uses supported SHA256 and UNKNOWN chunking function, existing chunks and their exact concatenated digest.

## Actual behavior

Under default configuration the proxy successfully splices a 2,720,000-byte blob. Both app and proxy SplitBlob and ByteStream return the expected mapping and bytes. Authoritative app FindMissingBlobs still reports the blob missing after ten seconds, including after proxy reads warm its local cache.

Two independent fixtures reproduced this in the isolated target (runs 1 and 2): https://app.buildbuddy.io/invocation/4e7d7c45-c0a4-4f29-8767-5cd7a4474a1e. Earlier default-config reproduction (also two fixtures): https://app.buildbuddy.io/invocation/0231ba28-f80b-47c0-9c46-6cf389d85a1a.

Persistent until a physical full-blob upload or relevant configuration change; no convergence within the ten-second observation window. Released CAS FindMissingBlobs uses ShouldUploadChunked to gate manifest fallback, which requires size > 4 * avg_chunk_size_bytes (4 MiB by default). SpliceBlob accepts this smaller valid blob, and ByteStream's fallback threshold is only 2 MiB. Thus successful bytes/mapping reads disagree with presence. This is not an auth, network or fixture failure: the same digest and fixture-local credentials succeed on both endpoints.

## Logs excerpt

```text
FindMissing divergence remains after 10.094910561s with default config; SplitBlob and ByteStream succeed
Should be empty, but was [hash:"b89d2075d7c5c6aea03ec1ac08b346420dc935ac34daef6362df4931b3afeddd" size_bytes:2720000]
```

Final-source re-verification: https://app.buildbuddy.io/invocation/3b45a649-b67f-46bb-8c6b-11c0f59559a9; both independent runs fail after 10.082830931s and 10.094910561s.

## Notes

No non-default component config, image overrides or fault injection. Full physical upload mitigates the mismatch; lowering the average chunk size also moves this digest above the presence fallback gate. The dedicated reproducer is tagged `bug` and excluded from normal CI. Run with `--test_tag_filters=bug --nocache_test_results`. Findings remain in this directory as required by task ownership; no generated index or shared files were edited.
