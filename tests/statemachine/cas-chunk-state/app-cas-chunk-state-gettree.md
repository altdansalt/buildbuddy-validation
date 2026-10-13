# GetTree cannot read a successfully spliced Directory

Title: GetTree omits chunk-manifest fallback
Components: app / cacheproxy
Image version/digest: enterprise-v2.314.0; app `gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843`; proxy `gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906`
Severity: medium
Severity justification: A valid published Directory can be downloaded but cannot be traversed by CAS GetTree, affecting consumers of directory trees. Requires chunk-only publication; no security boundary is implicated.
Status: confirmed
Repro test target: //tests/statemachine/cas-chunk-state:chunk_only_tree_repro
BuildBuddy invocation: https://app.buildbuddy.io/invocation/bac78b71-94d0-449c-90e4-f90bca9174ff

## Expected behavior

Successful SpliceBlob publishes a verified CAS Directory whose bytes are readable by every CAS read surface, including GetTree. No physical full-root upload should be necessary.

## Actual behavior

The proxy accepts a content-verified splice of a 2,720,000-byte Directory with six uploaded chunks. Authoritative app SplitBlob, FindMissingBlobs, BatchReadBlobs and ByteStream all return the expected state. App GetTree repeatedly returns FailedPrecondition: root digest not found.

Two independent fixtures reproduced this in the isolated target (runs 1 and 2): https://app.buildbuddy.io/invocation/bac78b71-94d0-449c-90e4-f90bca9174ff. Observation windows were 10.037536212s and 10.040445397s. Earlier cross-surface discovery: https://app.buildbuddy.io/invocation/4e2d9053-6dd8-4cb6-abe3-024d76b68f35.

This is a persistent lookup mismatch, not observed transient convergence: source `server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go` fetchDirectory directly calls cache.Get, while BatchRead/ByteStream reconstruct manifests. The UI BuildBuddyService.GetTree delegates to this CAS implementation. That UI failure follows from source; the isolated reproducer directly asserts the CAS failure. Successful physical-root coverage separately checks CAS and UI streaming transports.

## Logs excerpt

```text
GetTree divergence remains after 10.040445397s; SplitBlob, FindMissing, BatchRead and ByteStream succeed
rpc error: code = FailedPrecondition desc = Digest hash:"b89d2075d7c5c6aea03ec1ac08b346420dc935ac34daef6362df4931b3afeddd" size_bytes:2720000 not found
```

Final-source re-verification: https://app.buildbuddy.io/invocation/3b45a649-b67f-46bb-8c6b-11c0f59559a9; both independent runs fail at GetTree after 10.013619407s and 10.01072355s.

## Notes

Repro uses non-default `cache.avg_chunk_size_bytes: 524288` on both peers to place this small, CI-friendly Directory above the manifest-presence threshold. Other settings and images are harness defaults; no fault injection. Independence from this setting has not been experimentally established. The GetTree physical lookup itself is unconditional in released source.

Uploading the full Directory with BatchUpdateBlobs mitigates the failure. Normal coverage exercises this physical-root case explicitly; the failing conformance reproducer is tagged `bug`, keeping the default suite green. Run with `--test_tag_filters=bug --nocache_test_results`. Full configs/logs/image inspections are preserved by the fixture.
