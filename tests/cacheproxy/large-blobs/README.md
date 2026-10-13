# Large ByteStream blobs

Released-image tests stream deterministic data from disk in 1 MiB messages and
hash downloads without collecting a whole blob. Each app/proxy and
identity/zstd upload combination has different content, preventing early CAS
completion from bypassing coverage. Both endpoints verify full identity/zstd
reads and near-end offsets (including exact EOF); identity reads also exercise
limits beyond the remaining suffix. Sizes are 64 MiB, 256 MiB and 2 GiB, with a
free-disk guard before generating each size. Test payloads repeat pseudorandom
1 MiB blocks and are therefore compressible; these are not worst-case disk or
compression-ratio tests.

Containers have a 1536 MiB memory ceiling and swap disabled. Successful 2 GiB
transfers under that ceiling detect whole-blob buffering, but do not establish a
precise peak RSS or a constant-memory asymptotic bound. Caches are enlarged to
8 GiB to avoid default 1 GiB eviction interfering with large-blob validation.

Interrupted identity uploads query status and resume at a returned offset, or
restart at zero on NOT_FOUND/UNIMPLEMENTED. The pinned release explicitly does
not implement QueryWriteStatus; successful nonzero resumption is therefore not
claimed. Validation covers 64 MiB size/hash mismatch errors and negative read
parameters. Deliberate disk exhaustion, cache quota enforcement, concurrent
multi-GiB transfers, and interrupted compressed resumption remain follow-ups.

The `compressed_read_limit_test` target is a bug-tagged spec reproducer. See
`findings/cacheproxy-large-blobs-compressed-read-limit.md` for evidence.

Run coverage with `bb remote test //tests/cacheproxy/large-blobs/... --config=nobug`.
Use `--test_arg=-test.v` to see disk availability and any resource-based skips.
