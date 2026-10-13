# FindMissingBlobs reports a successfully spliced 2-4 MiB blob missing

Title: FindMissingBlobs reports a spliced blob (2 MiB < size <= 4 MiB) missing while ByteStream and SplitBlob serve it
Components: app
Image version/digest: enterprise-v2.314.0; app gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843
Severity: low
Severity justification: False CAS absence causes redundant uploads (and failed presence checks) for a blob that is verifiably readable. Needs an explicit SpliceBlob of a 2-4 MiB blob, not ordinary client chunking (which only chunks blobs > 4 MiB); a whole-blob upload makes it present.
Status: confirmed
Repro test target: //tests/statemachine/cas-chunk-state:splice_find_missing_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/f825b62b-c0fc-45c7-a552-c7912a5adec6

## Expected behavior

After a successful content-verified SpliceBlob, FindMissingBlobs must report the blob present, consistent with reads of the same digest.

## Actual behavior

Default config, no overrides. Four 680,000-byte chunks are uploaded and spliced into one 2,720,000-byte blob (SHA256). SpliceBlob succeeds; SplitBlob returns the 4 chunks and ByteStream Read returns the exact bytes (controls). FindMissingBlobs on the same app with the same credentials still lists the digest as missing for the whole 10 s polling window (not an asynchronous-visibility race within that window; not tested beyond it).

Reproduced twice, each in a fresh fixture:
- run 1: https://app.buildbuddy.io/invocation/4aa8c114-1ad6-419a-a50c-3b42d2dffe10
- run 2: https://app.buildbuddy.io/invocation/f825b62b-c0fc-45c7-a552-c7912a5adec6

## Logs excerpt

```text
Error:      	Should be empty, but was [hash:"2f012e77d15eea1ed1036599ceeb220bb6ec94c708c203eb0cd6cf28944429e4" size_bytes:2720000]
Messages:   	ByteStream and SplitBlob serve the spliced blob, so FindMissingBlobs must report it present
```

## Notes

Root cause (v2.314.0 source): SpliceBlob stores only a manifest, so presence relies on the manifest fallback in FindMissingBlobs, which is skipped when `d.GetSizeBytes() <= maxChunkSizeBytes` (`server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go:175`, threshold from line 155), where `MaxChunkSizeBytes() = 4 * avg_chunk_size_bytes` = 4 MiB by default (`server/remote_cache/chunking/chunking.go:73-75`). The read path falls back at a lower, independent threshold: `ShouldReadChunked` uses `min(cache.min_chunked_read_fallback_size_bytes (2 MiB), MaxChunkSizeBytes())` (`chunking.go:95-97`, `:130`). Blobs in (2 MiB, 4 MiB] are therefore readable but never reported present. The code comment says this is deliberate for blobs below the write threshold ("should be uploaded as whole blobs"), but SpliceBlob accepts such blobs, so the surfaces disagree. The divergence persists until the blob is uploaded whole or config changes; permanence beyond the 10 s window was not measured.

Related but not reproduced here (needs non-default `cache.avg_chunk_size_bytes`): GetTree on a chunk-only Directory root fails because its directory fetch has no manifest fallback (same file, ~`:553-561`). See also `findings/app-capabilities-small-splice-unreadable.md` (sub-2 MiB splices).
