# Successful small-blob splice does not make the blob readable

Title: SpliceBlob succeeds but ByteStream cannot read the resulting small blob
Components: app
Image version/digest: enterprise-v2.314.0; gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: medium
Severity justification: A client using advertised chunked upload receives success but the composed CAS blob is unavailable, potentially breaking downstream builds. Requires use of splice for blobs at or below the default 2 MiB read-fallback threshold; large blob reconstruction passed.
Status: confirmed
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
here. Independent verification confirms the defect; see below. Tests and findings remain in this repository.


## Verification

Verdict: **confirmed**, independently reviewed on 2026-10-13 against PR #82
head `74d691a`. Medium severity is appropriate: an advertised, successful upload
can leave a valid CAS blob unreadable and break consumers, but this requires
small-blob splicing and has a whole-blob upload workaround. No security impact
was established.

Both fresh runs reached the final read assertion with identical NOT_FOUND errors.
Before that assertion, the test checked advertised splice support, uploaded both
chunks, successfully spliced them, and fetched the exact ordered chunk mapping.
It uses one instance (the empty instance), SHA256 consistently, a bounded RPC
context, and the fixture's disposable CACHE_WRITE key. These controls rule out
read-only no-op writes, anonymous blackholing, wrong namespaces, missing chunks,
and readiness failures. The tiny input cannot plausibly exhaust the default
1 GiB cache; the deterministic source path explains the failure without an
asynchronous visibility race.

The applicable [REAPI v2.12 SpliceBlob contract](https://github.com/bazelbuild/remote-apis/blob/v2.12.0/build/bazel/remote/execution/v2/remote_execution.proto#L503-L559)
describes chunked upload, requires prior chunk upload and digest verification,
and permits no-op success when the blob or a usable construction mapping already
exists. Although it motivates the API with large blobs, it specifies no numeric
minimum size. [ByteStream read semantics](https://github.com/bazelbuild/remote-apis/blob/v2.12.0/build/bazel/remote/execution/v2/remote_execution.proto#L295)
allow fetching a CAS blob by its digest. An accepted mapping that cannot serve
that blob is inconsistent with successful chunked upload. The original report's
newer spec revision deprecates SpliceBlob; v2.12 is the direct reference for this
released unary API.

Pinned release source explains the inconsistency:
[spliceBlob](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go#L1262-L1333)
accepts two chunks without a minimum blob size and calls
[Manifest.Store](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/chunking/chunking.go#L298-L318),
which validates and saves an AC manifest rather than the whole CAS blob.
[ByteStream.Read](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go#L200-L213)
only reconstructs after a cache miss if
[ShouldReadChunked](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/chunking/chunking.go#L130-L132)
allows it; the default threshold is 2 MiB, exclusive. The test's six-byte blob
therefore cannot enter reconstruction. The release's
[RBE documentation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/docs/config-rbe.md#L20-L43)
says Bazel-side chunking is supported by default; it does not document a minimum
accepted splice size. The manifest salt guidance does not explain this result:
the same fixture successfully reads its mapping.

Executed both bug targets in each invocation using:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/capabilities:small_splice_bug_test //tests/app/capabilities:unknown_digest_bug_test --config=nobug --test_tag_filters= --nocache_test_results --test_output=all
```

- [Independent run 1](https://app.buildbuddy.io/invocation/48fd0cea-192c-4416-a0c8-ec98b991b148): final ByteStream read returned NOT_FOUND.
- [Independent run 2](https://app.buildbuddy.io/invocation/20c506ab-f45c-4667-a249-e9104fbb518b): same final assertion and NOT_FOUND.

Both logs identify the exact app digest recorded above. No image or config
overrides were used; Docker components ran inside the remote tests. These are
executed failures, not cached results or capability skips.
