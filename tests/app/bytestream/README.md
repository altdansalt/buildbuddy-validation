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
