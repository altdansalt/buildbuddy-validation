# Compressed GetFileRange rejects a documented URI representation

Title: Compressed GetFileRange rejects valid logical ranges
Components: app / cacheproxy (writer)
Image version/digest: enterprise-v2.314.0
- app: `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
- proxy: `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: low
Severity justification: Valid API clients cannot fetch compressed logical ranges; identity ranges and compressed full-file reads provide workarounds. No auth-boundary failure is claimed.
Status: confirmed
Repro test target: //tests/statemachine/convergence-streams:compressed_range_bug
BuildBuddy invocation: https://app.buildbuddy.io/invocation/1ec15f22-09c0-4419-bc39-cf55fe8ce6c9
Second invocation: https://app.buildbuddy.io/invocation/29045d85-fceb-4d11-aa01-67e88ff7dda8

## Expected behavior

`proto/api/v1/file.proto` explicitly documents `compressed-blobs/zstd` URIs for GetFileRange, logical inclusive start and exclusive end, and a maximum 16 MiB range. Read the valid [17,328) logical slice of a blob uploaded via proxy ByteStream.Write. Full compressed GetFile and identity GetFileRange are positive controls.

## Actual behavior

Both controls pass. Compressed GetFileRange fails over gRPC and REST, with unchanged bytes and valid authentication. This is a permanent implementation restriction, not a transient convergence delay. It uses the default fixture config and pinned images, without faults or external component requests. The historical audit also covered app/proxy writers with identity/zstd uploads in both representations; see AUDIT.md. The current isolated target uses one proxy zstd write to keep the repro minimal.

## Logs excerpt

```text
zstd=true: gRPC GetFileRange: rpc error: code = FailedPrecondition desc = passthrough compression does not support offset/limit
zstd=true: REST GetFileRange: GetFileRange HTTP 500: rpc error: code = FailedPrecondition desc = passthrough compression does not support offset/limit
```

## Notes

`enterprise/server/api/api_server.go:GetFileRange` passes the compressed resource and positive logical limit to cache.Reader. `enterprise/server/backends/pebble_cache/pebble_cache.go:readerForMetadata` rejects passthrough compressed ranges. ByteStream.Read instead transcodes logical ranges, which the green suite verifies on both app and proxy. No finite convergence is possible without changing this implementation or using identity URIs.

Reproduce with `bb remote test //tests/statemachine/convergence-streams:compressed_range_bug --test_tag_filters=bug --nocache_test_results --test_output=errors`. The contract assertion fails until fixed; `bug` excludes it from default CI. Findings remain in this directory as requested; no upstream report or shared files were changed.
