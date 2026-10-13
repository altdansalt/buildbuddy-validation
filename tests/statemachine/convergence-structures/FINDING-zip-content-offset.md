# ZIP manifest omits the documented content offset

Title: GetZipManifest returns content_offset zero for ZIP entries with nonzero data offsets
Components: app
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`
Severity: low
Severity justification: ZIP metadata consumers cannot use the documented content_offset to locate file bytes. The current frontend uses header_offset instead, so entry downloads still succeed.
Status: confirmed
Repro test target: //tests/statemachine/convergence-structures:zip_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/905ddbf5-2b8d-4e24-939d-395c9c709d76

## Expected behavior

`proto/zip.proto` ManifestEntry.content_offset documents the absolute byte offset of file content, distinct from header_offset. After a valid ZIP is uploaded via BatchUpdateBlobs, gRPC and HTTP GetZipManifest should expose that offset.

## Actual behavior

A valid single-entry ZIP containing payload.txt has content starting at byte 41, independently calculated by Go's archive/zip Reader.DataOffset. gRPC returns content_offset=0; JSON HTTP omits the field, which defaults to zero. ZIP entry download using the same returned ManifestEntry succeeds and returns exact bytes, ruling out an invalid archive or bad bytestream URI.

Two independent remote reproductions:
- https://app.buildbuddy.io/invocation/905ddbf5-2b8d-4e24-939d-395c9c709d76
- https://app.buildbuddy.io/invocation/77b0c1f3-662a-4fae-833c-ee32d7e28ae6 — includes successful entry-download positive control before the offset assertions.

Permanent metadata omission, not a transient app/proxy propagation delay: `server/util/ziputil/ziputil.go` ReadDirectoryHeader populates HeaderOffset and never assigns ContentOffset. No non-default config is required. There is no observed convergence time; waiting does not assign the field.

## Logs excerpt

```text
gRPC content_offset=0 want actual ZIP data offset 41
HTTP contentOffset=<nil> want actual ZIP data offset 41
```

## Notes

The app image and default cache/storage/auth are supplied by the fixture. All uploads/RPCs/HTTP downloads address the local app; no fault injection. The source's ZIP entry extraction path reads and validates the local header using HeaderOffset, which explains why bytes still download correctly. The green sequence asserts the known zero boundary alongside exact entry membership, uncompressed/compressed sizes, compression, CRC, header metadata agreement and entry bytes after every mutation. The bug-tagged isolated test asserts the documented content-offset behavior and is excluded from normal CI.

Run `bb remote test //tests/statemachine/convergence-structures:zip_repro --test_tag_filters=bug --nocache_test_results --test_output=errors`.
