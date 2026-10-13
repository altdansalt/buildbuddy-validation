# Compressed ByteStream reads accept a forbidden nonzero limit

Title: App and cache proxy accept nonzero read_limit for compressed blobs
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843; cacheproxy gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906
Severity: low
Severity justification: Authenticated clients receive a successful compressed partial response for a request that the protocol requires rejecting. This causes interoperability differences in error handling; no cross-group disclosure or corruption of stored blobs was observed.
Status: unverified
Repro test target: //tests/cacheproxy/large-blobs:compressed_read_limit_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/91ce27b0-cf86-49a7-9982-239c4a8bd6d9

## Expected behavior

The [Remote Execution API ByteStream compressed download contract](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) says: "Servers MUST return INVALID_ARGUMENT if ReadRequest.read_limit is non-zero."

After uploading a 64 MiB blob, reading its `compressed-blobs/zstd/<hash>/67108864` resource at uncompressed offset `67108847` with `read_limit=13` must return gRPC INVALID_ARGUMENT from both endpoints.

## Actual behavior

The app and proxy both accept the request and send data instead. First reproduction: [91ce27b0](https://app.buildbuddy.io/invocation/91ce27b0-cf86-49a7-9982-239c4a8bd6d9). Second reproduction: [28ad57d9](https://app.buildbuddy.io/invocation/28ad57d9-24a2-4495-ba1f-4455f068b317). The second run drains the stream to EOF and still sees successful completion, ruling out an error deferred until after the first response.

Both runs use fresh fixture-local app/proxy containers and their disposable authenticated key. A direct app upload succeeds before the invalid read. Valid compressed and uncompressed reads pass in the regression suite; this is not a missing compression capability or authentication failure. The direct app control reproduces independently of proxy forwarding.

## Logs excerpt

```text
--- FAIL: TestCompressedReadLimit/app
    compressed read with limit: <nil>
    expected: 0x3
    actual  : 0x0
--- FAIL: TestCompressedReadLimit/cacheproxy
    compressed read with limit: <nil>
    expected: 0x3
    actual  : 0x0
```

These are RPC assertions from the test output, not container panic messages. Full fixture logs/configs/inspect metadata are preserved in undeclared test outputs.

## Notes

Run `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/large-blobs:compressed_read_limit_test --test_tag_filters=bug --nocache_test_results --test_output=all`.

Defaults are unchanged except 8 GiB Pebble cache capacity and a 1536 MiB hard container memory limit with swap disabled; no image overrides. Source inspection at [v2.314.0 ByteStream server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go) shows `CheckReadPreconditions` rejecting negative offsets/limits but no rejection for positive compressed limits. `ReadCASResource` passes the limit to the identity cache reader before compressing its result. The [proxy implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go) uses that same local server or forwards the request upstream.

Status intentionally remains unverified for maintainer triage, as requested, despite two reproductions. Findings remain in this repository.
