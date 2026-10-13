# Documented compressed GetFileRange URI fails while compressed ByteStream range succeeds

Title: Documented compressed GetFileRange URI fails while compressed ByteStream range succeeds
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: A documented API read surface fails on valid stored compressed blobs; identity reads still work, limiting impact.
Status: confirmed
Repro test target: //tests/statemachine/bytestream-state:go_default_test --test_arg=-test.run=^TestCompressedAPIRangeDivergence$ --test_arg=-test.v
BuildBuddy invocations: https://app.buildbuddy.io/invocation/456f3afc-6cb5-4eac-a1e2-307652ba4406 and https://app.buildbuddy.io/invocation/c19f093e-dfd9-4258-9cd9-a0767725dbfa
Both full targeted suites passed; isolated known-difference probes reproduce the state differences without failing normal CI.

## Expected behavior

proto/api/v1/file.proto GetFileRangeRequest explicitly accepts compressed-blobs/zstd URIs and promises a compressed payload. A valid interior range should expose the same logical slice as the ByteStream range (after decompression).

## Actual behavior

For valid zstd uploads through app and proxy, identity API range and compressed ByteStream range return the model slice. Compressed API GetFileRange repeatedly returns FailedPrecondition: passthrough compression does not support offset/limit; HTTP API returns 500 with the same message. This is representation-dependent and persists across three spaced observations, rather than an eventual-publication delay.

## Logs excerpt

```text
writer=app compressed API range remains rejected after 304.714219ms; identity API and compressed ByteStream ranges succeed
writer=cacheproxy compressed API range remains rejected after 304.443513ms; identity API and compressed ByteStream ranges succeed
```

## Notes

Default configuration and pinned images. enterprise/server/api/api_server.go calls Cache.Reader directly; enterprise/server/backends/pebble_cache/pebble_cache.go readerForMetadata rejects offset/limit when requested and stored compressors are both ZSTD. ByteStream supports this by transcoding the requested logical range. Identity URI is a workaround.

The isolated test asserts the known difference so normal CI stays green. It must
be updated if the released behavior changes. No errors are ignored and no
component calls target shared deployments. Harness artifacts include container
configs and actual image IDs. Findings stay in this owned directory per task.

Full expanded suite passed: https://app.buildbuddy.io/invocation/456f3afc-6cb5-4eac-a1e2-307652ba4406

Final pinned-image full-suite verification passed in 217.6 seconds, including exact HTTP absence responses, ZIP JSON fields, required CLI reads and stable-state controls. No shared changes are required.
