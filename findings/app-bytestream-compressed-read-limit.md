# App accepts nonzero limits on compressed ByteStream reads

Title: Compressed ByteStream Read ignores REAPI nonzero-limit rejection
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: Protocol conformance failure on authenticated CAS reads; clients receive a successful compressed partial response for a request that must be rejected. No corruption, panic, or trust-boundary escape was observed.
Status: unverified
Duplicate of: [app-compression-read-limit.md](app-compression-read-limit.md) (confirmed). This finding is retained for its independent seeded randomized coverage.
Repro test target: //tests/app/bytestream:compressed_limit_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/2f2d8981-ba45-42be-b816-d21c398d4fe0

## Expected behavior

[REAPI compressed download rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto#L313-L317)
require `INVALID_ARGUMENT` whenever `ReadRequest.read_limit` is nonzero for a
`compressed-blobs/zstd` resource.

## Actual behavior

After a plain upload of `compressed limit conformance`, reading its zstd resource
with `ReadLimit: 1` returns `OK` and 14 wire bytes. A preceding zero-limit
compressed control read decompresses to the entire original blob successfully.

The minimal reproducer failed identically in two independent uncached runs:

- https://app.buildbuddy.io/invocation/2f2d8981-ba45-42be-b816-d21c398d4fe0
- https://app.buildbuddy.io/invocation/cde57969-ea1c-41a2-8199-28d2a4f48379

These use separate fixture-local apps and disposable authenticated keys. The
successful control rules out missing data, unsupported zstd, authentication,
and resource-name errors. Only the read limit changes between control and repro.
The seeded exploratory property test also observed this behavior across random
sizes, offsets and both upload formats. The status remains `unverified` as
requested for independent verification, despite two local-fixture reproductions.

## Logs excerpt

```text
compressed ReadLimit=1: got code=OK bytes=14 error=<nil>; want InvalidArgument
```

Both runs show the pinned image above. Preserved container logs are in the
invocation's undeclared test outputs; the cleanup panic scan reported no panic.

## Notes

No image, component config, or flag overrides. Uses the standard single-app
fixture (local Redis, SQLite and Pebble); no shared deployment is addressed.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:compressed_limit_bug_test --test_tag_filters=bug --nocache_test_results --test_output=errors
```

The passing randomized test restricts compressed limits to zero. Plain reads
continue to check randomized limits and negative-range status codes; past-EOF
positive offsets remain covered by the unchanged round 1 bug test. No upstream
report has been filed.
