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

Round 2 adds replayable mixed-batch properties (seed `2026101202`, 24 rounds
of 12 items) comparing each singleton upload's status with its shuffled mixed
batch status. Hash mismatches, size mismatches, identity and zstd uploads mix in
each trial; readback checks valid contents and rejected-item absence. Digests
are unique in this property so duplicate-key semantics cannot obscure the oracle.

Eight concurrent workers upload/read overlapping digests using identity and zstd;
acknowledged items must remain readable and every successful read must decode to
the original bytes. Final reads require every digest. SHA1/SHA384/SHA512 batch
round trips run only when advertised by the local app's capabilities, including
compressed uploads and rejected size mismatches. Unsupported compressors return
per-item UNIMPLEMENTED without affecting valid peers.

Storage-pressure coverage uses a 2 MiB cache budget and 12 MiB of seeded
incompressible data (`2026101203`). Reads permit eviction (NOT_FOUND) but never
corrupt successful responses; this does not assume immediate GC or enforce a
hard resident-byte quota. Each batch stays below the RPC size ceiling.

`//tests/app/cas-batch:malformed_zstd_bug_test` isolates the suspected malformed
zstd status classification defect: client data currently gets INTERNAL instead
of INVALID_ARGUMENT, while a valid peer succeeds. It is excluded by `nobug`.

Round 2 validation on 2026-10-13:

- [Passing full nobug suite](https://app.buildbuddy.io/invocation/d620b13c-cf87-4ff7-8f51-819fe753c201)
  (`--test_arg=-test.v`): all three non-default digest functions ran and passed.
- [Malformed-zstd minimal repro, run 1](https://app.buildbuddy.io/invocation/96cc87f3-6cca-46f5-9156-aa56a81b56f1)
  and [run 2](https://app.buildbuddy.io/invocation/fbe4e3da-10b5-4505-9324-c0a0e58628e1),
  both uncached and failing only the invalid-item status assertion.
