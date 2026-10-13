# Local audit (no push)

Audited origin/cand/sm1/bytestream-state at 9e95390.

Verdict: shallow relative to the stated every-surface requirement; assertions are real.

Only mutation: ByteStream.Write. Four distinct blobs (two writer components x two
initial encodings), each uploaded three times. Each completion checks both
components' identity/zstd ByteStream.Read bytes, FindMissingBlobs presence,
BatchReadBlobs status/count/decoded bytes, and GetMetadata digest size. App-only
checks assert UI gRPC/JSON metadata digest size, identity/zstd GetFile bytes,
one identity GetFileRange slice, and identity HTTP download/view bytes + 200.
CLI checks final model bytes against app/proxy, conditional on bb availability.

Missing: QueryWriteStatus (source returns Unimplemented), directory-shaped CAS
writes/GetTree on both components, UI GetTree/GetTreeDirectorySizes, API v1 HTTP
GetFile/GetFileRange, compressed HTTP URLs/ranges, browser request-context/cookie
download path, metadata stored-size/access/modify fields and compressor selection,
read offsets/limits, errors/unfinished uploads, and prior model keys after each
subsequent mutation. DeleteFile exists but is explicitly excluded by author.

No unconditional success paths: require assertions propagate errors. Send EOF
is accepted but CloseAndRecv must succeed and committed size is asserted.
Compressed -1 allows legitimate duplicate-upload short circuit. Polling masks
transient FindMissingBlobs differences for 10 seconds; it cannot waive persistent
ones. Missing list checks count rather than digest identity; BatchReadBlobs omits
returned digest and compressor enum validation. CLI can skip and only runs at end.

Three independent perturbations, all failed at intended assertions:
- missing count 1 -> 0: https://app.buildbuddy.io/invocation/7e25db21-04a4-4f20-a251-9d30d7a8e664
- reference bytes first bit flipped: https://app.buildbuddy.io/invocation/7b482c4e-3ff4-42ff-a51c-251037d7a080
- metadata expected digest size +1: https://app.buildbuddy.io/invocation/a47beaa7-8d87-4abb-9ab8-5596d48c3b12
The reference perturbation also failed CLI, proving bb was available.

New check: TestAuditMissingSurfaces uses valid directory protobuf uploads, CAS
GetTree, UI directory-size totals/count, identity/zstd metadata including stored
size/timestamps, and QueryWriteStatus. Cold proxy metadata after app upload returns
NotFound while app metadata succeeds, observed twice:
https://app.buildbuddy.io/invocation/a73a1859-2c6a-4fa0-b17e-7c9527ed9312
https://app.buildbuddy.io/invocation/ad0e1a21-5672-40bf-b22a-d2a6518f8806
First run also had an audit-check expectation error: proto comment promises
hash/size but directory-size implementation returns hash; fixed in second run.
This is a cross-surface state difference, not a confirmed product bug: cache
GetMetadata intentionally examines local backing storage, unlike read-through
ByteStream. Original test warms proxy with reads before metadata, masking it.

Images: enterprise-v2.314.0 defaults, app digest
sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843,
proxy digest sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906.

Final isolated cold-state check:
https://app.buildbuddy.io/invocation/dca00257-8d71-46ea-a931-1be492dec00f
Cold proxy metadata remained unavailable throughout 10 seconds. A successful
proxy ByteStream.Read immediately populated metadata: digest size 58000,
stored size 53, both identity and zstd metadata requests succeeded. Last-modify
and last-access timestamps differed from app because these are local replicas.
All other added assertions and restored TestPublication passed; only the
cold_metadata_agreement subtest failed. No product finding filed.
