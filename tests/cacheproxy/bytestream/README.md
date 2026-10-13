# ByteStream through the released cache proxy

Run passing coverage with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/bytestream/... --config=nobug
```

The fixture starts only the pinned app and cache proxy with their default local
Pebble caches and fixture-local authentication. No shared service receives RPCs.

Coverage includes cold/warm read ranges, read limits across frame boundaries,
EOF, negative parameters, missing blobs, invalid initial and subsequent write
offsets, omitted resource names on subsequent write messages, interrupted writes
restarted using the same upload name, concurrent 16 MiB uploads, direct upstream
readback, read cancellation, and full readback after cancellation.

Protocol references:

- [ByteStream protocol](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto)
- [Remote Execution CAS ByteStream contract](https://github.com/bazelbuild/remote-apis/blob/6def1c5d27a5/build/bazel/remote/execution/v2/remote_execution.proto)
- [Released proxy implementation](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/enterprise/server/byte_stream_server_proxy/byte_stream_server_proxy.go)
- [Released ByteStream server](https://github.com/buildbuddy-io/buildbuddy/blob/v2.314.0/server/remote_cache/byte_stream_server/byte_stream_server.go)

The pinned app deliberately returns UNIMPLEMENTED for QueryWriteStatus; the
restart regression records this behavior and restarts at zero. It does not
claim that nonzero resumable uploads are supported. Send alone does not prove
that partial bytes were committed. Cancellation assertions apply to the client
stream; full readback verifies subsequent requests and stored data remain sound.
EOF beyond the resource size is tested separately against the normative
OUT_OF_RANGE requirement.

Round 2 adds `TestSeededProxyAppEquivalence`: 24 replayable random blobs with
random upload chunkings (including empty initial messages), offsets, and limits.
It compares response status codes and accumulated bytes against the direct app,
checks valid reads against the original bytes, and exercises app-only blobs before
and after warming the proxy. Negative ranges and missing blobs are also compared.
Known beyond-EOF defects stay in the separate bug target; completed-write status
is likewise tracked separately. The findings link the equivalent app reports on
PR #37 and remain unverified pending triage.

The default seed is 20261012; replay or explore another seed with:

```sh
GIT_CONFIG_GLOBAL=/dev/null bb remote test //tests/cacheproxy/bytestream:go_default_test --config=nobug --test_arg=-test.run=TestSeededProxyAppEquivalence --test_arg=-bytestream.seed=20261012 --nocache_test_results
```
