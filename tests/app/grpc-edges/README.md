# App gRPC edge coverage

Run `GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/app/grpc-edges/... --config=nobug` after configuring the public fetch remote as described in the root AGENTS.md.

All RPCs target the digest-pinned app started by the fixture. The suite exercises:

- Forward-compatible unknown protobuf fields.
- Invalid tags, truncated varints, and illegal wire types.
- Exactly 50,000,000 bytes and one byte over the default receive limit.
- Repeated ASCII metadata and binary metadata containing non-UTF-8 bytes.
- Concurrent successful and expired calls sharing a gRPC connection.
- Direct cleartext HTTP/2 requests, bare and explicit protobuf content types,
  unsupported content types, expired and malformed timeout headers, and unsupported compression.

Successful capability calls after rejection batches check that the app remains usable.
The large requests use unknown fields so transport limits are isolated from application
batch limits. No receive-limit override is used: the released server chooses the maximum
of the current and deprecated flags, whose defaults are both 50,000,000 bytes.

Behavior references:

- [Pinned gRPC server configuration](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/grpc_server/grpc_server.go)
- [Pinned capabilities handler](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go)
- [Remote Execution API capabilities contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
- [gRPC HTTP/2 protocol](https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md)
- [gRPC status mapping](https://github.com/grpc/grpc/blob/master/doc/statuscodes.md)

The separate `invalid_utf8_test` target is tagged `bug` and expects the server to reject
invalid UTF-8 in a known proto3 string field. Run it with
`--test_tag_filters=bug --nocache_test_results`.

The malformed-request tests expect INTERNAL because gRPC explicitly maps request-proto
parsing failures to that status; this alone is not evidence of an application defect.
Expired ordinary client calls cover client cancellation and connection reuse. The raw
HTTP/2 `grpc-timeout: 1n` case independently exercises the server's timeout handling.
