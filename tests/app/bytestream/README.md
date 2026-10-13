# App ByteStream validation

These tests start only the pinned released app and its local Redis sidecar using
`harness.Start`; all protocol calls use the fixture-local authenticated client.

Run passing coverage:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream/... --config=nobug
```

Run each defect reproducer explicitly:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:query_status_bug_test --test_tag_filters=bug --nocache_test_results
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:read_past_end_bug_test --test_tag_filters=bug --nocache_test_results
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:unfinished_response_bug_test --test_tag_filters=bug --nocache_test_results
```

The assertions follow the [ByteStream protocol](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto)
and [REAPI CAS resource and transfer rules](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto).
Implementation inspection uses the released
[v2.314.0 ByteStream server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go),
not a locally built component.

Regression coverage includes offset/limit readback across multiple responses,
end-of-file reads, negative ranges, malformed and missing resources, write name
and offset validation, digest/size mismatch, omitted subsequent resource names,
empty data frames, a separate empty finish frame, interrupted upload restart,
and eight concurrent uploads with distinct upload IDs of the same digest.
Restarting an interrupted upload at zero is exercised as recovery coverage; it
does not assert that the server persists partial upload bytes. QueryWriteStatus
must provide the authoritative resume offset even when that offset is zero.

Round 2 adds `randomized_test`, a deterministic property test (default seed
`20261012`). It mixes empty/tiny blobs, sizes around 64 KiB and 1 MiB, and fifteen
random sizes up to 2 MiB, with alternating random and repetitive contents.
Both plain and zstd uploads use random chunk boundaries, empty frames, and a
separate finish frame. Each is read in both formats against the original byte
slice oracle, including random offsets/limits, EOF, negative ranges, and the
zero limits for compressed reads. A new bug-tagged minimal test checks the
REAPI prohibition on nonzero compressed read limits. The fixture cleanup checks
preserved app logs for panics. Positive offsets past EOF remain in the unchanged
`read_past_end_bug_test` while that known defect is independently verified.

Replay or explore another seed:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:randomized_test --config=nobug --nocache_test_results --test_arg=-bytestream.seed=20261012
```

New round 2 defect reproducer:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:compressed_limit_bug_test --test_tag_filters=bug --nocache_test_results
```

Round 3 adds `concurrent_property_test`, using the same replay seed. Eight
rounds race four writers (plain and zstd, distinct upload IDs) with four readers
of the same digest. Readers check random ranges in both formats against the
original bytes, allow only empty `NotFound` before publication, and require
continued visibility after a successful read. Half the rounds start with the
digest already published. Sixteen more rounds interrupt each upload three times
at seeded wire offsets, alternating cancellation and half-close, then restart
from zero using the same upload resource. Explicit zero-byte and full-data
interruptions supplement random cuts. Unfinished uploads must remain absent;
completed restarts must return the exact original bytes. Concurrent scheduling
is intentionally uncontrolled; all payloads, chunks and ranges are seeded.

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/bytestream:concurrent_property_test --config=nobug --nocache_test_results --test_arg=-bytestream.seed=20261013
```

The compressed-limit finding is a duplicate of
`findings/app-compression-read-limit.md`; its independent coverage is retained.
