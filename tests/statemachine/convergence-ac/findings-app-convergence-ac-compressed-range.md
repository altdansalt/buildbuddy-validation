# Documented compressed GetFileRange URI rejects an available CAS blob

Title: GetFileRange rejects compressed URIs while identity ranges and compressed ByteStream reads succeed
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: The documented compressed file-range API cannot read an otherwise available valid blob. Callers can use the identity URI or compressed ByteStream reads. This is a functionality inconsistency, not an authorization or data-loss finding.
Status: confirmed
Repro test target: //tests/statemachine/convergence-ac:compressed_range_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/25b637c8-1f20-4441-a983-8b2faa72d413
Second BuildBuddy invocation: https://app.buildbuddy.io/invocation/fdf39dbb-d386-4c0b-aec5-038578fcaa1a

## Expected behavior

`proto/api/v1/file.proto` explicitly documents replacing `/blobs/` with
`/compressed-blobs/zstd/` in GetFileRange URIs to receive a compressed payload.
The valid full range `start=0,end=digest.size_bytes` is below 16 MiB. After a
successful BatchUpdateBlobs or ByteStream.Write, decompressing this range should give the same
reference bytes as identity GetFileRange and compressed ByteStream.Read.

## Actual behavior

A 576-byte valid CAS blob is uploaded successfully. The parameterized repro
uses BatchUpdateBlobs and compressed ByteStream.Write in separate fresh app
fixtures. Identity GetFileRange
returns all 576 reference bytes; compressed ByteStream.Read decompresses to the
same reference bytes. Compressed GetFileRange returns gRPC FailedPrecondition
and HTTP 500 with `passthrough compression does not support offset/limit`.
The first reproduction made 61 calls over 15.068708158 seconds, all rejected.
The second reproduction made 61 calls over 15.078086843 seconds, all rejected.

Confirmed initial independent invocations:

- https://app.buildbuddy.io/invocation/7dc49275-232a-4af6-804b-4ff7441d2708
- https://app.buildbuddy.io/invocation/fc5ac879-495d-4676-84e2-0dc176a4f65c

The expanded two-transport reproduction also failed independently in
https://app.buildbuddy.io/invocation/25b637c8-1f20-4441-a983-8b2faa72d413:
BatchUpdateBlobs: 61 calls / 15.074096432 seconds; ByteStream.Write: 61 calls /
15.073158453 seconds, with identical gRPC/HTTP rejection.
The repeated expanded reproduction
https://app.buildbuddy.io/invocation/fdf39dbb-d386-4c0b-aec5-038578fcaa1a
again rejected both transports: BatchUpdateBlobs 61 calls / 15.075864138 seconds;
ByteStream.Write 61 calls / 15.072576373 seconds.

## Logs excerpt

```text
compressed GetFileRange after successful upload: attempts=61 observation=15.068708158s last=rpc error: code = FailedPrecondition desc = passthrough compression does not support offset/limit
HTTP compressed GetFileRange: status=500 body=rpc error: code = FailedPrecondition desc = passthrough compression does not support offset/limit
expected: 200
actual  : 500
```

## Notes

The fixture uses default app cache/API configuration: local Pebble, SHA256,
`cache.pebble.min_bytes_auto_zstd_compression=100` (released default). No
non-default flag, version override, fault injection or shared service is used.
Disposable local authentication succeeds on the control reads. The test starts
only the released app image inside the remote runner; no proxy/executor is
needed for this minimal API repro.

Source inspection explains a permanent rejection for this stored representation,
rather than a delayed propagation effect: `enterprise/server/api/api_server.go`
passes a positive uncompressed range limit to `Cache.Reader`, while
`enterprise/server/backends/pebble_cache/pebble_cache.go:readerForMetadata`
rejects any offset/limit when requested and stored compressors are the same
non-IDENTITY value. The bounded observations cannot establish an infinite
runtime duration, but no convergence occurred during the 15-second window.

Run explicitly with `bb remote test //tests/statemachine/convergence-ac:compressed_range_repro --test_tag_filters=bug --nocache_test_results`.
The independent `bug` tag excludes this expected-failing target from normal CI.
The expanded passing suite uses identity GetFileRange plus compressed GetFile
and ByteStream reads; it does not silently accept the compressed range failure.
