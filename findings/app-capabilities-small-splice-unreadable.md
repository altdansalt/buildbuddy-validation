# Successful small-blob splice does not make the blob readable

Title: SpliceBlob succeeds but ByteStream cannot read the resulting small blob
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: A client using advertised chunked upload receives success but the composed CAS blob is unavailable, potentially breaking downstream builds. Requires use of splice for blobs at or below the default 2 MiB read-fallback threshold; large blob reconstruction passed.
Status: unverified
Repro test target: //tests/app/capabilities:small_splice_bug_test
BuildBuddy invocation: https://app.buildbuddy.io/invocation/98db1138-3ea8-4f2c-af34-3dc839ccdc7e

## Expected behavior

After uploading all chunks and successfully splicing their digests, a client
should be able to read the resulting blob. There is no advertised minimum splice
size. An unsupported size should be rejected rather than accepted as a successful
upload. [REAPI chunk upload contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).

## Actual behavior

Upload `abc` and `def`, then splice them into the digest of `abcdef`. Splice
succeeds, and SplitBlob returns the two correct chunk digests, but ByteStream
Read for the six-byte result returns NOT_FOUND. Reproduced independently in the
invocation above and
https://app.buildbuddy.io/invocation/3b5c6667-e0e0-44b6-a69e-be83100bb129;
both use --nocache_test_results and init-dockerd=true.

Authentication, local CAS writes and manifest registration all succeed in the
same test. The passing seeded property uses the identical operations on blobs
between 2 and 3 MiB, ruling out missing service/auth support. Passing invocation:
https://app.buildbuddy.io/invocation/2f94fba4-bdba-405b-9b26-0b3e34861ca9.
Ordinary blobs need not have a split mapping; their allowed NOT_FOUND response is
not classified as a defect.

## Logs excerpt

```text
spliced blob read: data="" error=rpc error: code = NotFound desc = key "PTdefault/bef57ec7f53a6d40beb640a780a639c83bc29ac8a9816f1fc6c5c6dcd93c4721/1/cas/v5" not found
successful splice must make the blob readable
```

## Notes

Default fixture configuration, no image overrides. Release source
`server/remote_cache/chunking/chunking.go` defines
`cache.min_chunked_read_fallback_size_bytes=2097152`; `ShouldReadChunked` only
attempts manifest reconstruction above this threshold. SpliceBlob stores the
manifest without materializing the whole CAS blob. This explains the size
boundary. Clients can avoid small-blob splicing and upload the entire blob.
Lowering that configuration threshold is a potential workaround, not validated
here. Status remains unverified pending maintainer review, as requested for
round 2. Tests and findings remain in this repository.
