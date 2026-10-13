# App CAS batch conformance

All RPCs target an authenticated, fixture-local released app container. The
normal target covers hash and size mismatches in mixed uploads, duplicate
uploads/reads, empty blobs and requests, NOT_FOUND reads, missing checks after
rejected writes, and batch limits (including a near-limit upload/read).
Assertions compare digest fields rather than protobuf runtime state; response
ordering is deliberately not assumed.

Sources inspected for enterprise-v2.314.0:

- [Released CAS implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/content_addressable_storage_server/content_addressable_storage_server.go)
- [Released capabilities](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/capabilities_server/capabilities_server.go)
- [Released gRPC budget](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/util/rpcutil/rpcutil.go)
- [Remote Execution API](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)
  (consulted 2026-10-12): BatchUpdateBlobs digest mismatch errors are per item;
  BatchReadBlobs individual errors belong in the corresponding status. Batch
  capabilities value zero leaves the size ceiling to the protocol. The passing test also records that uploads above 4 MB succeed, while reads
  above the released CAS implementation's 4 MB budget require ByteStream.

Run coverage with `bb remote test //tests/app/cas-batch/... --config=nobug`.
Run reproducers with `bb remote test //tests/app/cas-batch/...
--test_tag_filters=bug --nocache_test_results --test_output=all`.
Use the public fetch remote and `GIT_CONFIG_GLOBAL=/dev/null` per root AGENTS.md.
